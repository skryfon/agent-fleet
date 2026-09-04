// Command control-plane is the AgentFleet control plane: the only writer to
// Postgres, owner of the task/run state machine, policy engine, and the
// runner-facing + human-facing HTTP API described in development-plan.md §4.
//
// This process runs the HTTP server (internal/api), the outbox relay
// (internal/outbox), internal/supervisor's run.launch/run.kill handlers,
// and internal/questions' timeout-ladder sweeper together;
// internal/reconcile's sweep (P8) joins this same errgroup once it exists.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"agentfleet/internal/api"
	"agentfleet/internal/budget"
	"agentfleet/internal/fanout"
	"agentfleet/internal/outbox"
	"agentfleet/internal/policy"
	"agentfleet/internal/questions"
	"agentfleet/internal/redact"
	"agentfleet/internal/store"
	"agentfleet/internal/supervisor"
	"agentfleet/internal/zulip"
)

const shutdownTimeout = 10 * time.Second

func main() {
	// deploy/compose.yaml's healthcheck runs this binary with -healthcheck
	// instead of a second process: distroless-static has no shell, curl, or
	// wget to run a CMD-SHELL probe with, so the binary probes itself.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(selfCheck(addrFromEnv() + "/readyz"))
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("control-plane: fatal", "error", err)
		os.Exit(1)
	}
}

func addrFromEnv() string {
	addr := os.Getenv("CONTROL_PLANE_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	return "http://localhost" + addr
}

// selfCheck GETs url and returns a shell-style exit code — 0 for a 2xx
// response, 1 otherwise. distroless-static has no separate binary to probe
// with, so -healthcheck makes this same binary do it.
func selfCheck(url string) int {
	client := http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 1
	}

	return 0
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	redactor := redact.FromEnv(os.LookupEnv,
		"DATABASE_URL", "GH_TOKEN", "OMNI_ROUTE_API_KEY", "ADMIN_TOKEN", "SUPERVISOR_SECRET", "BRIDGE_SECRET",
	).WithLiterals(
		// M6 per-project credentials (development-plan.md §7 M6): every
		// GH_TOKEN_<SLUG> this process's own environment carries (deploy/
		// compose.yaml passes the same set to both control-plane and
		// supervisor) — their names aren't known ahead of time the way the
		// fixed list above is, so FromEnv can't see them.
		redact.EnvValuesWithPrefix(os.Environ(), "GH_TOKEN_")...,
	)

	addr := os.Getenv("CONTROL_PLANE_ADDR")
	if addr == "" {
		addr = ":8080" //nolint:goconst // matches addrFromEnv's own default; not worth a shared constant for one literal
	}

	srv := &api.Server{
		Store:            st,
		Redact:           redactor,
		Log:              log,
		AdminToken:       os.Getenv("ADMIN_TOKEN"),
		SupervisorSecret: os.Getenv("SUPERVISOR_SECRET"),
		// M6's .agentfleet/project.yaml compiler will replace this
		// process-wide Manifest with one resolved per-project — see
		// internal/api.Server.Manifest's doc comment. Every role but
		// orchestrator/implementer still denies every mediated tool
		// (fail-closed). D7 (docs/adr/0007) says only the orchestrator gets
		// ask_human: DefaultRole below is "implementer" for a project with
		// no manifest (or one naming more than one agent), and its
		// MediatedTools list deliberately omits ask_human — see the M5
		// comment on that row.
		Manifest: policy.Manifest{
			Roles: map[string]policy.Role{
				// M5: orchestrator gains spawn_worker (fan-out) and
				// answer_worker (D7's ask_orchestrator round trip). Still
				// keeps ask_human — it's the only role that does (D7).
				"orchestrator": {MediatedTools: []string{"ask_human", "spawn_worker", "answer_worker", "gh_pr_create", "pr_opened", "report_deviation"}},
				// gh_pr_create/pr_opened (M4): the mediated PR-creation round
				// trip af-github's gh_pr_create now makes (runner/packages/
				// af-github) — see internal/policy's package doc for why
				// even an allow-listed tool still passes through here.
				// M5: ask_orchestrator/report_to_orchestrator replace
				// ask_human here — D7 says a worker never reaches Zulip
				// directly; see docs/adr/0007's "dormant until M5" note,
				// which this manifest is what actually makes true.
				"implementer": {MediatedTools: []string{"ask_orchestrator", "report_to_orchestrator", "gh_pr_create", "pr_opened", "report_deviation"}},
			},
		},
		// M4 hard-kill caps (development-plan.md §5's manifest example: "budget:
		// { usd: 8, minutes: 45, questions: 3 }"). Process-wide until M6's
		// manifest compiler owns per-project caps — same documented stand-in
		// as Manifest above.
		BudgetCaps: budget.Caps{USD: 8, Minutes: 45, Questions: 3},
		// M5 fan-out caps (development-plan.md §7 M5: "spawn_worker with
		// depth and fan-out limits"). Process-wide until M6's manifest
		// compiler, same documented stand-in as Manifest/BudgetCaps above.
		FanoutCaps: fanout.Caps{MaxDepth: 3, MaxChildrenPerRun: 4, MaxActiveSubtree: 12},
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	relay := outbox.NewRelay(st, outbox.DefaultConfig(), log)

	supervisorURL := os.Getenv("SUPERVISOR_URL")
	if supervisorURL == "" {
		supervisorURL = "http://supervisor:8090"
	}

	supervisorHandlers := &supervisor.Handlers{
		Store:  st,
		Daemon: supervisor.NewClient(supervisorURL, srv.SupervisorSecret, nil),
		// SUPERVISOR_SECRET doubles as the run-token derivation key (see
		// supervisor.Handlers.RunTokenSecret's doc comment) — already
		// shared with cmd/supervisor for the container-report callback's
		// own auth, so no new secret is provisioned for it.
		RunTokenSecret: srv.SupervisorSecret,
		DefaultRole:    "implementer",
		DefaultModel:   "agy/gemini-3.6-flash-low",
	}

	relay.Handle("run.launch", supervisorHandlers.RunLaunch)
	relay.Handle("run.kill", supervisorHandlers.RunKill)

	bridgeURL := os.Getenv("BRIDGE_URL")
	if bridgeURL == "" {
		bridgeURL = "http://bridge:8091"
	}

	zulipHandlers := &zulip.Handlers{
		Store:         st,
		Client:        zulip.NewClient(bridgeURL, os.Getenv("BRIDGE_SECRET"), nil),
		DefaultStream: os.Getenv("ZULIP_STREAM"),
	}

	relay.Handle("zulip.question", zulipHandlers.Notify)
	relay.Handle("zulip.review", zulipHandlers.Notify)
	relay.Handle("zulip.failed", zulipHandlers.Notify)
	relay.Handle("zulip.violation", zulipHandlers.Notify)

	sweeper := &questions.Sweeper{
		Store: st, Redact: redactor, Zulip: zulipHandlers.Client, Log: log, Config: questions.DefaultConfig(),
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return relay.Run(gctx)
	})

	g.Go(func() error {
		return sweeper.Run(gctx)
	})

	g.Go(func() error {
		log.Info("control-plane: listening", "addr", addr)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}

		return nil
	})

	g.Go(func() error {
		<-gctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		return httpServer.Shutdown(shutdownCtx)
	})

	return g.Wait()
}
