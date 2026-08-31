// Command control-plane is the AgentFleet control plane: the only writer to
// Postgres, owner of the task/run state machine, policy engine, and the
// runner-facing + human-facing HTTP API described in development-plan.md §4.
//
// This process runs the HTTP server (internal/api), the outbox relay
// (internal/outbox), and internal/supervisor's run.launch/run.kill
// handlers together; internal/reconcile's sweep (P8) joins this same
// errgroup once it exists.
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
	"agentfleet/internal/outbox"
	"agentfleet/internal/policy"
	"agentfleet/internal/redact"
	"agentfleet/internal/store"
	"agentfleet/internal/supervisor"
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
		"DATABASE_URL", "GH_TOKEN", "OMNI_ROUTE_API_KEY", "ADMIN_TOKEN", "SUPERVISOR_SECRET",
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
		// internal/api.Server.Manifest's doc comment. An empty Manifest
		// denies every mediated tool for every role (internal/policy's
		// unknown_role/not_allow_listed defaults), which is the correct
		// fail-closed starting point before that compiler exists.
		Manifest: policy.Manifest{},
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
	// zulip.notify (M3) has no registered handler yet —
	// internal/outbox.Relay's own documented behavior for an unregistered
	// topic is to poison the row immediately (see relay.go's dispatchOne),
	// which is the correct, visible failure mode until that handler lands,
	// not a silent drop.

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return relay.Run(gctx)
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
