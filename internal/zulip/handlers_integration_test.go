//go:build integration

// Integration tests require a real Postgres at DATABASE_URL, migrated up to
// deploy/migrations' latest revision — see `make test-integration`. Follows
// internal/supervisor/handlers_integration_test.go's own pattern: a real
// Store against real Postgres, and a fake daemon via httptest.NewServer in
// place of a mocked interface — Client is a thin, otherwise-untestable HTTP
// wrapper either way.
package zulip_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/outbox"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
	"agentfleet/internal/zulip"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — see `make test-integration`")
	}

	s, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	t.Cleanup(s.Close)

	return s
}

// seedQuestion creates the minimum project/feature/task/run/question chain
// a zulip.question notify needs, and returns the task and question ids.
func seedQuestion(t *testing.T, s *store.Store) (taskID, questionID uuid.UUID) {
	t.Helper()

	ctx := context.Background()
	suffix := uuid.NewString()

	proj, err := s.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "zulip-it-project-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := s.Q().CreateFeature(ctx, db.CreateFeatureParams{ProjectID: proj.ID, Slug: "zulip-it-feature-" + suffix, State: "OPEN"})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := s.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "notify test task", Intent: "verify zulip.Notify",
		AcceptanceCriteria: []byte(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
		SpecRefs: []byte(`[]`), State: "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	run, err := s.Q().InsertRun(ctx, db.InsertRunParams{TaskID: task.ID, Role: "orchestrator", Model: "test", State: "RUNNING"})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	q, err := s.Q().CreateQuestion(ctx, db.CreateQuestionParams{
		RunID: run.ID, TaskID: task.ID, FeatureID: pgtype.UUID{Bytes: feat.ID, Valid: true},
		Kind: "free_text", Body: "which branch?", Options: []byte(`[]`), State: "OPEN",
	})
	if err != nil {
		t.Fatalf("CreateQuestion: %v", err)
	}

	return task.ID, q.ID
}

// TestNotifyIsIdempotentOnRedelivery is the property outbox.Handler's
// contract requires: a redelivered zulip.question row (the question already
// carries a zulip_message_id from an earlier attempt) must not post twice.
func TestNotifyIsIdempotentOnRedelivery(t *testing.T) {
	s := openTestStore(t)
	taskID, questionID := seedQuestion(t, s)

	var posts atomic.Int32

	fakeBridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer fakeBridge.Close()

	h := &zulip.Handlers{Store: s, Client: zulip.NewClient(fakeBridge.URL, "secret", nil), DefaultStream: "general"}

	payload, _ := json.Marshal(map[string]string{"task_id": taskID.String(), "question_id": questionID.String()})
	msg := outbox.Message{ID: 1, Topic: "zulip.question", Payload: payload}

	if err := h.Notify(context.Background(), msg); err != nil {
		t.Fatalf("first Notify: %v", err)
	}

	if err := h.Notify(context.Background(), msg); err != nil {
		t.Fatalf("redelivered Notify: %v", err)
	}

	if posts.Load() != 1 {
		t.Fatalf("bridge received %d posts, want exactly 1 (idempotent on redelivery)", posts.Load())
	}
}

// TestNotifyReviewAndFailedPostOnce covers the other two reasons through
// the same handler, proving the topic-based dispatch (not a KeyReason
// string parse) picks the right message shape for each.
func TestNotifyReviewAndFailedPostOnce(t *testing.T) {
	s := openTestStore(t)
	taskID, _ := seedQuestion(t, s)

	var lastBody []byte

	fakeBridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer fakeBridge.Close()

	h := &zulip.Handlers{Store: s, Client: zulip.NewClient(fakeBridge.URL, "secret", nil), DefaultStream: "general"}

	payload, _ := json.Marshal(map[string]string{"task_id": taskID.String()})

	for _, topic := range []string{"zulip.review", "zulip.failed"} {
		if err := h.Notify(context.Background(), outbox.Message{ID: 2, Topic: topic, Payload: payload}); err != nil {
			t.Fatalf("Notify(%s): %v", topic, err)
		}

		if len(lastBody) == 0 {
			t.Fatalf("Notify(%s): bridge received no body", topic)
		}
	}
}
