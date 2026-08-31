// Package supervisor implements the run.launch/run.kill outbox handlers
// (development-plan.md §2, §7 M2/P5): it runs INSIDE the control-plane
// process (registered on internal/outbox.Relay by cmd/control-plane) and
// talks to the separate cmd/supervisor daemon over HTTP. It never touches
// Postgres's outbox/event tables directly beyond the plain run-table reads
// and the one INSERT a launch needs — no state transition, no event, is
// recorded here; those already happen via internal/api's
// POST /v1/runs/{id}/container handler when the daemon calls back.
//
// This package holds no Podman access itself (docs/adr/0011, D11): that
// lives in internal/podman, imported only by cmd/supervisor.
package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client is a thin HTTP client for the cmd/supervisor daemon's own API
// (not to be confused with internal/podman.Client, which the daemon uses
// against the Podman socket).
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

// NewClient returns a Client for the daemon at baseURL (e.g.
// "http://supervisor:8090"), authenticating with the same SUPERVISOR_SECRET
// the daemon uses to call back into the control plane's authSupervisor
// routes (internal/api/middleware.go).
func NewClient(baseURL, secret string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{baseURL: baseURL, secret: secret, http: httpClient}
}

// LaunchRequest is the daemon's POST /launch body — everything
// internal/podman.Spec needs to create and start one runner container.
type LaunchRequest struct {
	RunID   string `json:"run_id"`
	Token   string `json:"token"` // plaintext per-run bearer token, never logged or persisted here
	Task    string `json:"task"`  // the prompt handed to the headless agent
	RepoURL string `json:"repo_url"`
	Role    string `json:"role"`
}

// KillRequest is the daemon's POST /kill body.
type KillRequest struct {
	RunID string `json:"run_id"`
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var reader io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("supervisor client: marshaling %s body: %w", path, err)
		}

		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("supervisor client: building %s request: %w", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.secret)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("supervisor client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("supervisor client: %s %s: status %d: %s", method, path, resp.StatusCode, msg)
	}

	return nil
}

// Launch asks the daemon to create and start one runner container. A
// non-2xx response (including 429 at MAX_CONCURRENT_RUNS) is returned as an
// error, which internal/outbox.Relay's retry/backoff turns into exactly the
// queueing behavior a concurrency cap needs, with no separate queue to
// build.
func (c *Client) Launch(ctx context.Context, req LaunchRequest) error {
	return c.do(ctx, http.MethodPost, "/launch", req)
}

// Kill asks the daemon to remove a run's container, if any. The daemon
// treats "no such container" as success, not an error.
func (c *Client) Kill(ctx context.Context, runID string) error {
	return c.do(ctx, http.MethodPost, "/kill", KillRequest{RunID: runID})
}
