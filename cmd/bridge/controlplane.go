package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// controlPlaneClient is the bridge's outbound calls back into the control
// plane — identity resolution and open-question lookup (both read-only,
// authAdmin-gated per internal/api/api.go's route comments) and the answer
// itself. The bridge holds no database access of its own
// (development-plan.md §2: "bridge ... Stateless"), so every decision that
// needs state crosses this boundary.
type controlPlaneClient struct {
	baseURL    string
	adminToken string
	http       *http.Client
}

func newControlPlaneClient(baseURL, adminToken string, httpClient *http.Client) *controlPlaneClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &controlPlaneClient{baseURL: baseURL, adminToken: adminToken, http: httpClient}
}

// errNotFound is returned by identity/question lookups on a 404 — the
// caller's job is to log-and-ignore (identity) or skip (no open question),
// never to treat it as an infrastructure failure.
type errNotFound struct{ what string }

func (e errNotFound) Error() string { return e.what + " not found" }

func (c *controlPlaneClient) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("control-plane client: marshaling %s body: %w", path, err)
		}

		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("control-plane client: building %s request: %w", path, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.adminToken)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control-plane client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("control-plane client: reading %s %s response: %w", method, path, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound{what: path}
	}

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("control-plane client: %s %s: status %d: %s", method, path, resp.StatusCode, raw)
	}

	return raw, nil
}

type identity struct {
	DisplayName string  `json:"display_name"`
	ZulipUserID *string `json:"zulip_user_id"`
	Role        string  `json:"role"`
}

// GetIdentityByZulip resolves a Zulip sender to a known identity —
// development-plan.md §6: "Verify the sender maps to a known identity ...
// Unmapped senders are ignored and logged." Returns errNotFound for an
// unmapped sender, never a bare error.
func (c *controlPlaneClient) GetIdentityByZulip(ctx context.Context, zulipUserID string) (identity, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/identities/by-zulip/"+url.PathEscape(zulipUserID), nil)
	if err != nil {
		return identity{}, err
	}

	var id identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return identity{}, fmt.Errorf("control-plane client: parsing identity: %w", err)
	}

	return id, nil
}

type openQuestion struct {
	ID string `json:"id"`
}

// GetOpenQuestionByTopic resolves a Zulip topic to the one question
// question_one_open_per_feature_uk guarantees is its only OPEN one.
// Returns errNotFound when the topic has no open question.
func (c *controlPlaneClient) GetOpenQuestionByTopic(ctx context.Context, topic string) (openQuestion, error) {
	raw, err := c.do(ctx, http.MethodGet, "/v1/questions?zulip_topic="+url.QueryEscape(topic), nil)
	if err != nil {
		return openQuestion{}, err
	}

	var q openQuestion
	if err := json.Unmarshal(raw, &q); err != nil {
		return openQuestion{}, fmt.Errorf("control-plane client: parsing question: %w", err)
	}

	return q, nil
}

// AnswerQuestion calls POST /v1/questions/{id}/answer — the same endpoint a
// direct API caller uses, per internal/api/questions.go's own doc comment
// on why identity verification lives here in the bridge, not there.
func (c *controlPlaneClient) AnswerQuestion(ctx context.Context, questionID, answer, answeredBy string) error {
	_, err := c.do(ctx, http.MethodPost, "/v1/questions/"+url.PathEscape(questionID)+"/answer", map[string]string{
		"answer": answer, "answered_by": answeredBy,
	})

	return err
}
