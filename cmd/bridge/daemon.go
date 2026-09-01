package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// daemon holds every dependency the HTTP handlers need — mirrors
// cmd/supervisor's own daemon struct shape.
type daemon struct {
	cfg          config
	zulip        *zulipClient
	controlPlane *controlPlaneClient
	log          *slog.Logger
}

func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", d.healthz)
	mux.HandleFunc("POST /notify", d.auth(d.notify))

	return mux
}

// auth gates every route but /healthz behind BRIDGE_SECRET — mirrors
// cmd/supervisor/daemon.go's auth exactly (not shared as a package for a
// ~10-line check used by two binaries).
func (d *daemon) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(d.cfg.secret)) != 1 {
			http.Error(w, "missing or invalid bridge secret", http.StatusUnauthorized)

			return
		}

		next(w, r)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok && tok != "" {
		return tok, true
	}

	return "", false
}

func (d *daemon) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// notifyRequest mirrors internal/zulip.NotifyRequest — this daemon
// deliberately doesn't import that package (its only caller is an HTTP
// client, not a Go dependency; the two processes' only contract is this
// wire shape), matching cmd/supervisor/daemon.go's own launchRequest
// convention.
type notifyRequest struct {
	Topic   string `json:"topic"`
	Content string `json:"content"`
}

func (d *daemon) notify(w http.ResponseWriter, r *http.Request) {
	var req notifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)

		return
	}

	if req.Topic == "" || req.Content == "" {
		http.Error(w, "topic and content are required", http.StatusBadRequest)

		return
	}

	if err := d.zulip.SendMessage(r.Context(), d.cfg.stream, req.Topic, req.Content); err != nil {
		d.log.Error("bridge: sending zulip message failed", "topic", req.Topic, "error", err)
		http.Error(w, "sending zulip message failed", http.StatusBadGateway)

		return
	}

	w.WriteHeader(http.StatusAccepted)
}
