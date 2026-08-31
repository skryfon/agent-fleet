// Command control-plane is the AgentFleet control plane: the only writer to
// Postgres, owner of the task/run state machine, policy engine, and the
// runner-facing + human-facing HTTP API described in development-plan.md §4.
//
// This process runs the HTTP server (internal/api) and the outbox relay
// (internal/outbox) together; internal/reconcile's sweep (P8) and
// internal/supervisor's run.launch/run.kill handlers (P5) join this same
// errgroup once they exist.
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
)

const shutdownTimeout = 10 * time.Second

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("control-plane: fatal", "error", err)
		os.Exit(1)
	}
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
		addr = ":8080"
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
	// run.launch, run.kill (internal/supervisor, P5) and zulip.notify
	// (M3) have no registered handler yet — internal/outbox.Relay's own
	// documented behavior for an unregistered topic is to poison the row
	// immediately (see relay.go's dispatchOne), which is the correct,
	// visible failure mode until those handlers land, not a silent drop.

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
