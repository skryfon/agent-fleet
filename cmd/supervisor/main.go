// Command supervisor launches and kills runner containers over a rootless
// Podman socket. It is the only AgentFleet service with Podman access
// (development-plan.md §2, D11) — see internal/podman, which it is the
// only importer of. internal/supervisor's run.launch/run.kill outbox
// handlers, running inside cmd/control-plane, are this daemon's only
// caller; it never touches Postgres itself.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"agentfleet/internal/podman"
)

const shutdownTimeout = 10 * time.Second

func main() {
	// deploy/compose.yaml's healthcheck runs this binary with -healthcheck
	// instead of a second process: distroless-static has no shell, curl, or
	// wget to run a CMD-SHELL probe with, so the binary probes itself.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(selfCheck("http://localhost" + getenvDefault("SUPERVISOR_ADDR", ":8090") + "/healthz"))
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("supervisor: fatal", "error", err)
		os.Exit(1)
	}
}

// selfCheck GETs url and returns a shell-style exit code — 0 for a 2xx
// response, 1 otherwise. Mirrors cmd/control-plane's own -healthcheck; not
// shared as a package for one ~10-line probe used by two binaries.
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

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	d := &daemon{
		cfg:    cfg,
		podman: podman.NewClient(cfg.podmanSocket),
		http:   &http.Client{Timeout: 30 * time.Second},
		sem:    make(chan struct{}, cfg.maxConcurrentRuns),
		log:    log,
	}

	// Startup reap runs once, synchronously, before serving — a run whose
	// container this daemon lost track of across a restart must be
	// resolved before anything else can race it (ListActiveRuns's own doc
	// comment names this daemon's "own startup reap" as one of its two
	// readers).
	if err := d.reap(ctx); err != nil {
		log.Error("supervisor: startup reap failed, continuing anyway", "error", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           d.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		log.Info("supervisor: listening", "addr", cfg.addr)

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

// config is every env var this daemon reads, gathered once at startup so a
// missing required value fails fast instead of mid-request.
type config struct {
	addr              string
	secret            string
	controlPlaneURL   string
	podmanSocket      string
	runnerImage       string
	runnerNetwork     string
	maxConcurrentRuns int
	ghToken           string
	omniRouteAPIKey   string
}

func loadConfig() (config, error) {
	cfg := config{
		addr:            getenvDefault("SUPERVISOR_ADDR", ":8090"),
		secret:          os.Getenv("SUPERVISOR_SECRET"),
		controlPlaneURL: os.Getenv("CONTROL_PLANE_URL"),
		podmanSocket:    getenvDefault("PODMAN_SOCKET", "/run/podman/podman.sock"),
		runnerImage:     getenvDefault("RUNNER_IMAGE", "agentfleet-runner"),
		runnerNetwork:   getenvDefault("RUNNER_NETWORK", "agentfleet_runners"),
		ghToken:         os.Getenv("GH_TOKEN"),
		omniRouteAPIKey: os.Getenv("OMNI_ROUTE_API_KEY"),
	}

	for name, v := range map[string]string{
		"SUPERVISOR_SECRET": cfg.secret,
		"CONTROL_PLANE_URL": cfg.controlPlaneURL,
		"GH_TOKEN":          cfg.ghToken,
	} {
		if v == "" {
			return config{}, errors.New(name + " is required")
		}
	}

	cfg.maxConcurrentRuns = 3 // development-plan.md §8: "ceiling is human review capacity", not CPU

	if v := os.Getenv("MAX_CONCURRENT_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return config{}, errors.New("MAX_CONCURRENT_RUNS must be a positive integer")
		}

		cfg.maxConcurrentRuns = n
	}

	return cfg, nil
}

func getenvDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return def
}
