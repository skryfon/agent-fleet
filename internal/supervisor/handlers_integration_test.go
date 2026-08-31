//go:build integration

// Integration tests require a real Postgres at DATABASE_URL, migrated up to
// deploy/migrations' latest revision — see `make test-integration`.
package supervisor_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"agentfleet/internal/outbox"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
	"agentfleet/internal/supervisor"
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

// seedTask creates the minimum project/feature/task chain run.launch needs,
// with a unique slug so parallel/repeated runs never collide.
func seedTask(t *testing.T, s *store.Store) uuid.UUID {
	t.Helper()

	ctx := context.Background()
	suffix := uuid.NewString()

	proj, err := s.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "sup-it-project-" + suffix, ManifestRef: "ref", ManifestHash: "hash",
		Repos: []string{"https://github.com/example/repo.git"}, Status: "ACTIVE",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := s.Q().CreateFeature(ctx, db.CreateFeatureParams{
		ProjectID: proj.ID, Slug: "sup-it-feature-" + suffix, State: "OPEN",
	})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := s.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "supervisor integration test task",
		Intent: "verify run.launch", AcceptanceCriteria: json.RawMessage(`["it launches"]`),
		Touches: []string{}, DependsOn: []uuid.UUID{}, SpecRefs: json.RawMessage(`[]`),
		State: "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	return task.ID
}

// TestRunLaunchIdempotentOnRedelivery is this plan's own verification item:
// a redelivered run.launch (the same outbox message, handled twice — the
// relay's actual retry path when a handler fails after partially
// succeeding) must create exactly one run row, thanks to
// run_active_per_task_uk plus GetActiveRunForTask's redelivery check.
func TestRunLaunchIdempotentOnRedelivery(t *testing.T) {
	s := openTestStore(t)
	taskID := seedTask(t, s)

	var launchCalls atomic.Int32

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		launchCalls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer daemon.Close()

	h := &supervisor.Handlers{
		Store:          s,
		Daemon:         supervisor.NewClient(daemon.URL, "test-secret", nil),
		RunTokenSecret: "test-secret",
		DefaultRole:    "implementer",
		DefaultModel:   "test-model",
	}

	payload, _ := json.Marshal(map[string]string{"task_id": taskID.String()})
	msg := outbox.Message{Topic: "run.launch", Payload: payload}

	if err := h.RunLaunch(context.Background(), msg); err != nil {
		t.Fatalf("RunLaunch (first): %v", err)
	}

	if err := h.RunLaunch(context.Background(), msg); err != nil {
		t.Fatalf("RunLaunch (redelivery): %v", err)
	}

	if got := launchCalls.Load(); got != 2 {
		t.Errorf("expected the daemon to be called twice (idempotent retry, not skipped), got %d", got)
	}

	run, err := s.Q().GetActiveRunForTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetActiveRunForTask: %v", err)
	}

	// run_active_per_task_uk (0002_control_plane.up.sql) makes a second
	// concurrent PENDING/STARTING/RUNNING run for the same task impossible
	// at the schema level — this just confirms exactly one row exists by
	// construction, not via a count query.
	if run.TaskID != taskID {
		t.Fatalf("unexpected run for task: %+v", run)
	}
}

// TestRunLaunchTokenNeverPersistedAsEvent is the canary check
// development-plan.md §8 requires for every secret this codebase mints:
// run.launch's per-run bearer token must never land in the event table,
// since it never goes through internal/redact — RunLaunch writes no event
// at all, only a run row and an outbound HTTP call, and this test pins that
// invariant so a future change that adds an event here is forced to also
// add redaction.
func TestRunLaunchTokenNeverPersistedAsEvent(t *testing.T) {
	s := openTestStore(t)
	taskID := seedTask(t, s)

	var gotToken string

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotToken = body.Token
		w.WriteHeader(http.StatusAccepted)
	}))
	defer daemon.Close()

	h := &supervisor.Handlers{
		Store: s, Daemon: supervisor.NewClient(daemon.URL, "test-secret", nil),
		RunTokenSecret: "test-secret", DefaultRole: "implementer", DefaultModel: "test-model",
	}

	payload, _ := json.Marshal(map[string]string{"task_id": taskID.String()})

	if err := h.RunLaunch(context.Background(), outbox.Message{Topic: "run.launch", Payload: payload}); err != nil {
		t.Fatalf("RunLaunch: %v", err)
	}

	if gotToken == "" {
		t.Fatalf("daemon never received a token")
	}

	events, err := s.Q().ListEventsByTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("ListEventsByTask: %v", err)
	}

	for _, ev := range events {
		if strings.Contains(string(ev.Payload), gotToken) {
			t.Fatalf("run token leaked into event %d payload: %s", ev.ID, ev.Payload)
		}
	}
}
