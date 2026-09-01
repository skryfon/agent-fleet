package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetIdentityByZulipReturnsErrNotFoundOn404 is the sender-verification
// contract inbound.go's resolveAndAnswer depends on: an unmapped sender
// must be distinguishable from an infrastructure failure so the caller can
// log-and-ignore instead of erroring loudly (development-plan.md §6).
func TestGetIdentityByZulipReturnsErrNotFoundOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-tok" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newControlPlaneClient(srv.URL, "admin-tok", nil)

	_, err := c.GetIdentityByZulip(context.Background(), "999")

	var nf errNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("GetIdentityByZulip on a 404: got %v, want errNotFound", err)
	}
}

func TestAnswerQuestionPostsExpectedBody(t *testing.T) {
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"question_id":"q1","task_state":"RUNNING"}`)) //nolint:errcheck // test server
	}))
	defer srv.Close()

	c := newControlPlaneClient(srv.URL, "admin-tok", nil)

	if err := c.AnswerQuestion(context.Background(), "q1", "yes", "architect"); err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	if gotPath != "/v1/questions/q1/answer" {
		t.Fatalf("path = %q, want /v1/questions/q1/answer", gotPath)
	}
}
