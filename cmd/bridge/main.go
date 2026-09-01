// Command bridge translates Zulip events to and from the control-plane API
// (development-plan.md §2: "Zulip ↔ control-plane translation. Stateless.").
// It is the only AgentFleet service holding the real Zulip bot credential
// (ZULIP_BOT_API_KEY) — mirrors cmd/supervisor's own isolation story for
// Podman access: internal/zulip (inside cmd/control-plane) never talks to
// Zulip Cloud directly, only to this daemon's POST /notify over HTTP.
//
// Two independent halves run in this one process: the HTTP server (outbound
// notify requests from the control plane) and the inbound poller (Zulip's
// own real-time events API, translating replies/reactions into
// POST /v1/questions/{id}/answer calls back to the control plane).
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
)

const shutdownTimeout = 10 * time.Second

func main() {
	// Mirrors cmd/control-plane and cmd/supervisor's own -healthcheck
	// convention — distroless-static has no shell/curl/wget for a
	// CMD-SHELL probe.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(selfCheck("http://localhost" + getenvDefault("BRIDGE_ADDR", ":8091") + "/healthz"))
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("bridge: fatal", "error", err)
		os.Exit(1)
	}
}

func selfCheck(url string) int {
	client := http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(url) //nolint:gosec,noctx // localhost self-probe, no untrusted input
	if err != nil {
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 1
	}

	return 0
}

// config is every env var this daemon reads, gathered once at startup so a
// missing required value fails fast instead of mid-request — mirrors
// cmd/supervisor's own config/loadConfig shape.
type config struct {
	addr            string
	secret          string
	controlPlaneURL string
	adminToken      string

	zulipSite   string
	zulipEmail  string
	zulipAPIKey string
	stream      string
}

func loadConfig() (config, error) {
	cfg := config{
		addr:            getenvDefault("BRIDGE_ADDR", ":8091"),
		secret:          os.Getenv("BRIDGE_SECRET"),
		controlPlaneURL: os.Getenv("CONTROL_PLANE_URL"),
		adminToken:      os.Getenv("ADMIN_TOKEN"),
		zulipSite:       os.Getenv("ZULIP_SITE"),
		zulipEmail:      os.Getenv("ZULIP_BOT_EMAIL"),
		zulipAPIKey:     os.Getenv("ZULIP_BOT_API_KEY"),
		stream:          getenvDefault("ZULIP_STREAM", "general"),
	}

	for name, v := range map[string]string{
		"BRIDGE_SECRET":     cfg.secret,
		"CONTROL_PLANE_URL": cfg.controlPlaneURL,
		"ADMIN_TOKEN":       cfg.adminToken,
		"ZULIP_SITE":        cfg.zulipSite,
		"ZULIP_BOT_EMAIL":   cfg.zulipEmail,
		"ZULIP_BOT_API_KEY": cfg.zulipAPIKey,
	} {
		if v == "" {
			return config{}, errors.New(name + " is required")
		}
	}

	return cfg, nil
}

func getenvDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return def
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	d := &daemon{
		cfg:          cfg,
		zulip:        newZulipClient(cfg.zulipSite, cfg.zulipEmail, cfg.zulipAPIKey, nil),
		controlPlane: newControlPlaneClient(cfg.controlPlaneURL, cfg.adminToken, nil),
		log:          log,
	}

	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           d.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		log.Info("bridge: listening", "addr", cfg.addr)

		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}

		return nil
	})

	g.Go(func() error {
		return d.runInbound(gctx)
	})

	g.Go(func() error {
		<-gctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		return httpServer.Shutdown(shutdownCtx)
	})

	return g.Wait()
}
