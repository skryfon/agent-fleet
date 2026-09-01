package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPollEventsReRegistersOnExpiry proves the errBadEventQueue signal
// actually fires on Zulip's documented BAD_EVENT_QUEUE_ID response
// (deploy/zulip/README.md §3b), the one behavior inboundSession's caller
// relies on to know when to RegisterQueue again instead of retrying the
// same (dead) queue.
func TestPollEventsReRegistersOnExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"result": "error", "msg": "Bad event queue id", "code": "BAD_EVENT_QUEUE_ID",
		})
	}))
	defer srv.Close()

	c := newZulipClient(srv.URL, "bot@example.com", "key", nil)

	_, _, err := c.PollEvents(context.Background(), "stale-queue", 0)
	if err != errBadEventQueue { //nolint:errorlint // exact sentinel identity, not a wrapped chain
		t.Fatalf("PollEvents on an expired queue: got %v, want errBadEventQueue", err)
	}
}

// TestSendMessageSuccess proves the request shape (basic auth, form-encoded
// stream/topic/content) against a fake server that only accepts exactly
// that shape.
func TestSendMessageSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "bot@example.com" || pass != "key" {
			t.Errorf("unexpected auth: %s/%s (ok=%v)", user, pass, ok)
		}

		if err := r.ParseForm(); err != nil {
			t.Fatalf("parsing form: %v", err)
		}

		if r.PostForm.Get("to") != "general" || r.PostForm.Get("topic") != "feat-x" || r.PostForm.Get("content") != "hi" {
			t.Errorf("unexpected form: %+v", r.PostForm)
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"result": "success"})
	}))
	defer srv.Close()

	c := newZulipClient(srv.URL, "bot@example.com", "key", nil)

	if err := c.SendMessage(context.Background(), "general", "feat-x", "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}
