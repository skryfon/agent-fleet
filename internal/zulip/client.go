// Package zulip is the control-plane-side half of the M3 Zulip bridge:
// Client is a thin HTTP client to the SEPARATE cmd/bridge daemon (which
// holds the real ZULIP_BOT_API_KEY), and Handlers.Notify is the
// zulip.question/zulip.review/zulip.failed outbox handler that calls it —
// the same two-process shape internal/supervisor + cmd/supervisor already
// establishes for Podman access (development-plan.md §2: "bridge ...
// Stateless"; D11's "no privileged host socket to guard" reasoning applies
// equally to not handing every control-plane process the Zulip credential).
package zulip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client is a thin HTTP client for cmd/bridge's own API (not Zulip's) —
// mirrors internal/supervisor.Client's shape exactly.
type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

// NewClient returns a Client for the bridge daemon at baseURL (e.g.
// "http://bridge:8091"), authenticating with BRIDGE_SECRET.
func NewClient(baseURL, secret string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{baseURL: baseURL, secret: secret, http: httpClient}
}

// NotifyRequest is the bridge daemon's POST /notify body.
type NotifyRequest struct {
	Topic   string `json:"topic"`
	Content string `json:"content"`
}

// Notify asks the bridge to post one message to the configured stream under
// Topic. A non-2xx response is returned as an error, which
// internal/outbox.Relay's retry/backoff turns into exactly the right
// behavior for a transient Zulip outage — no separate retry queue to build.
func (c *Client) Notify(ctx context.Context, req NotifyRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("zulip client: marshaling notify body: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/notify", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("zulip client: building notify request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.secret)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("zulip client: notify: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("zulip client: notify: status %d: %s", resp.StatusCode, msg)
	}

	return nil
}
