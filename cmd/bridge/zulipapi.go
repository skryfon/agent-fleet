package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// zulipClient is a hand-rolled wrapper over Zulip Cloud's plain REST + real-
// time events API (deploy/zulip/README.md §7): no maintained Go client
// exists, and the handful of calls this bridge needs don't earn one.
type zulipClient struct {
	site   string // e.g. https://<org>.zulipchat.com
	email  string
	apiKey string
	http   *http.Client
}

func newZulipClient(site, email, apiKey string, httpClient *http.Client) *zulipClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &zulipClient{site: strings.TrimRight(site, "/"), email: email, apiKey: apiKey, http: httpClient}
}

// errBadEventQueue is PollEvents' sentinel for Zulip's BAD_EVENT_QUEUE_ID
// response (deploy/zulip/README.md §3b: "the queue expired ... and needs
// re-registering") — the inbound poller's loop distinguishes this from any
// other failure to know when to re-register instead of just retrying.
var errBadEventQueue = errors.New("zulip: event queue expired (BAD_EVENT_QUEUE_ID)")

func (c *zulipClient) do(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	var body io.Reader

	target := c.site + path
	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("zulip: building %s %s: %w", method, path, err)
	}

	req.SetBasicAuth(c.email, c.apiKey)

	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zulip: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("zulip: reading %s %s response: %w", method, path, err)
	}

	var envelope struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("zulip: %s %s: parsing response: %w", method, path, err)
	}

	if envelope.Result != "success" {
		if envelope.Code == "BAD_EVENT_QUEUE_ID" {
			return nil, errBadEventQueue
		}

		return nil, fmt.Errorf("zulip: %s %s: %s (%s)", method, path, envelope.Msg, envelope.Code)
	}

	return raw, nil
}

// SendMessage posts one stream message (deploy/zulip/README.md §3a).
func (c *zulipClient) SendMessage(ctx context.Context, stream, topic, content string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/messages", url.Values{
		"type":    {"stream"},
		"to":      {stream},
		"topic":   {topic},
		"content": {content},
	})

	return err
}

// registerResponse is /api/v1/register's shape, trimmed to the two fields
// PollEvents needs.
type registerResponse struct {
	QueueID     string `json:"queue_id"`
	LastEventID int64  `json:"last_event_id"`
}

// RegisterQueue opens an event queue narrowed to stream, per
// deploy/zulip/README.md §3b.
func (c *zulipClient) RegisterQueue(ctx context.Context, stream string, eventTypes []string) (registerResponse, error) {
	eventTypesJSON, _ := json.Marshal(eventTypes)
	narrowJSON, _ := json.Marshal([][]string{{"channel", stream}})

	raw, err := c.do(ctx, http.MethodPost, "/api/v1/register", url.Values{
		"event_types": {string(eventTypesJSON)},
		"narrow":      {string(narrowJSON)},
	})
	if err != nil {
		return registerResponse{}, err
	}

	var resp registerResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return registerResponse{}, fmt.Errorf("zulip: parsing register response: %w", err)
	}

	return resp, nil
}

// zulipEvent is the subset of a Zulip event's fields the inbound poller
// reads — message (a reply) and reaction (an emoji answer) are the two
// kinds development-plan.md §6 names ("Emoji reaction for choice/confirm;
// topic reply for free_text").
type zulipEvent struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Message *struct {
		ID       int64  `json:"id"`
		SenderID int64  `json:"sender_id"`
		Subject  string `json:"subject"` // Zulip's own field name for "topic"
		Content  string `json:"content"`
	} `json:"message,omitempty"`
	// Reaction events (type="reaction") carry these top-level, not nested.
	MessageID  int64  `json:"message_id,omitempty"`
	UserID     int64  `json:"user_id,omitempty"`
	EmojiName  string `json:"emoji_name,omitempty"`
	ReactionOp string `json:"op,omitempty"` // "add" | "remove" — only "add" answers a question
}

// PollEvents long-polls GET /api/v1/events (deploy/zulip/README.md §3b).
// Returns errBadEventQueue when the caller must RegisterQueue again.
func (c *zulipClient) PollEvents(ctx context.Context, queueID string, lastEventID int64) ([]zulipEvent, int64, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/events", url.Values{
		"queue_id":      {queueID},
		"last_event_id": {strconv.FormatInt(lastEventID, 10)},
		"dont_block":    {"false"},
	})
	if err != nil {
		return nil, lastEventID, err
	}

	var resp struct {
		Events []zulipEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, lastEventID, fmt.Errorf("zulip: parsing events response: %w", err)
	}

	newLastEventID := lastEventID
	for _, ev := range resp.Events {
		if ev.ID > newLastEventID {
			newLastEventID = ev.ID
		}
	}

	return resp.Events, newLastEventID, nil
}

// GetMessageTopic resolves a reaction event's message id back to the topic
// it was posted in — a reaction event carries no topic of its own.
func (c *zulipClient) GetMessageTopic(ctx context.Context, messageID int64) (string, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/messages/"+strconv.FormatInt(messageID, 10), nil)
	if err != nil {
		return "", err
	}

	var resp struct {
		Message struct {
			Subject string `json:"subject"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("zulip: parsing message response: %w", err)
	}

	return resp.Message.Subject, nil
}
