//go:build integration

// Integration tests require a real Postgres at DATABASE_URL, migrated up to
// deploy/migrations' latest revision. `make test-integration` documents the
// setup; CI's sqlc-verify job runs this file directly (see .github/workflows/ci.yml)
// since it already has a live, migrated Postgres for `sqlc diff`.
package store_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"agentfleet/internal/redact"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
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

// seedTaskAndRun creates the minimum project/feature/task/run chain a test
// needs, each with a unique slug so parallel/repeated test runs never
// collide on the unique constraints.
func seedTaskAndRun(t *testing.T, s *store.Store) (taskID, runID uuid.UUID) {
	t.Helper()

	ctx := context.Background()
	suffix := uuid.NewString()

	proj, err := s.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug:         "it-project-" + suffix,
		ManifestRef:  "ref",
		ManifestHash: "hash",
		Repos:        []string{},
		Status:       "ACTIVE",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := s.Q().CreateFeature(ctx, db.CreateFeatureParams{
		ProjectID: proj.ID,
		Slug:      "it-feature-" + suffix,
		State:     "OPEN",
	})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := s.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID:          feat.ID,
		Lane:               "direct",
		Title:              "integration test task",
		Intent:             "verify the store layer",
		AcceptanceCriteria: json.RawMessage(`[]`),
		Touches:            []string{},
		DependsOn:          []uuid.UUID{},
		SpecRefs:           json.RawMessage(`[]`),
		State:              "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	run, err := s.Q().InsertRun(ctx, db.InsertRunParams{
		TaskID: task.ID,
		Role:   "implementer",
		Model:  "test-model",
		State:  "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	return task.ID, run.ID
}

// seedTaskOnFeature creates a second RUNNING task+run pair on an existing
// feature — for tests that need two tasks sharing one Zulip topic (M3's
// question_one_open_per_feature_uk), where seedTaskAndRun's own fresh
// project/feature per call would defeat the point.
func seedTaskOnFeature(t *testing.T, s *store.Store, featureID uuid.UUID) (taskID, runID uuid.UUID) {
	t.Helper()

	ctx := context.Background()

	task, err := s.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID:          featureID,
		Lane:               "direct",
		Title:              "second integration test task",
		Intent:             "verify per-feature question uniqueness",
		AcceptanceCriteria: json.RawMessage(`[]`),
		Touches:            []string{},
		DependsOn:          []uuid.UUID{},
		SpecRefs:           json.RawMessage(`[]`),
		State:              "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertTask (second): %v", err)
	}

	run, err := s.Q().InsertRun(ctx, db.InsertRunParams{
		TaskID: task.ID, Role: "implementer", Model: "test-model", State: "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertRun (second): %v", err)
	}

	return task.ID, run.ID
}

// TestAppendMirrorEventsIdempotentReplay is docs/adr/0001's named hard
// invariant, exercised through the real Go store layer (not just raw SQL):
// posting the identical batch twice — the af-control retry-after-crash
// scenario — must insert each (run_id, seq) row exactly once.
func TestAppendMirrorEventsIdempotentReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	taskID, runID := seedTaskAndRun(t, s)

	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	params := db.AppendMirrorEventsParams{
		RunID:    runID,
		TaskID:   taskID,
		Seqs:     []int64{1, 2, 3},
		Kinds:    []string{"tool/call", "tool/result", "assistant/message"},
		Actors:   []string{"agent", "tool", "agent"},
		Payloads: [][]byte{[]byte(`{}`), []byte(`{}`), []byte(`{}`)},
		Ats:      []pgtype.Timestamptz{now, now, now},
	}

	first, err := s.Q().AppendMirrorEvents(ctx, params)
	if err != nil {
		t.Fatalf("first AppendMirrorEvents: %v", err)
	}

	if first != 3 {
		t.Fatalf("first batch: got %d rows inserted, want 3", first)
	}

	// The replay: byte-identical params, as af-control's retry does — seq
	// is never renumbered.
	second, err := s.Q().AppendMirrorEvents(ctx, params)
	if err != nil {
		t.Fatalf("replayed AppendMirrorEvents: %v", err)
	}

	if second != 0 {
		t.Fatalf("replayed batch: got %d rows inserted, want 0 (ON CONFLICT DO NOTHING)", second)
	}

	hw, err := s.Q().MirrorHighWaterSeq(ctx, runID)
	if err != nil {
		t.Fatalf("MirrorHighWaterSeq: %v", err)
	}

	if hw != 3 {
		t.Fatalf("high water seq = %d, want 3", hw)
	}
}

// TestAppendMirrorEventsPartialOverlapReplay covers the more realistic
// crash scenario: a batch that partially overlaps a previous one (some seqs
// already landed, some didn't) must accept only the new ones.
func TestAppendMirrorEventsPartialOverlapReplay(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	taskID, runID := seedTaskAndRun(t, s)

	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}

	first, err := s.Q().AppendMirrorEvents(ctx, db.AppendMirrorEventsParams{
		RunID: runID, TaskID: taskID,
		Seqs: []int64{1, 2}, Kinds: []string{"a", "b"}, Actors: []string{"agent", "agent"},
		Payloads: [][]byte{[]byte(`{}`), []byte(`{}`)}, Ats: []pgtype.Timestamptz{now, now},
	})
	if err != nil || first != 2 {
		t.Fatalf("seed batch: rows=%d err=%v", first, err)
	}

	// seq 2 overlaps (already landed), seq 3 is new.
	second, err := s.Q().AppendMirrorEvents(ctx, db.AppendMirrorEventsParams{
		RunID: runID, TaskID: taskID,
		Seqs: []int64{2, 3}, Kinds: []string{"b", "c"}, Actors: []string{"agent", "agent"},
		Payloads: [][]byte{[]byte(`{}`), []byte(`{}`)}, Ats: []pgtype.Timestamptz{now, now},
	})
	if err != nil {
		t.Fatalf("overlapping batch: %v", err)
	}

	if second != 1 {
		t.Fatalf("overlapping batch: got %d rows inserted, want 1 (only seq=3 is new)", second)
	}
}

// TestWithTxRollsBackOnError verifies WithTx's whole reason for existing:
// if fn returns an error, nothing it wrote is visible afterward — the same
// property ApplyTaskTransition (P3) will rely on for atomically committing
// a state change with its event and outbox rows.
func TestWithTxRollsBackOnError(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	slug := "it-rollback-" + uuid.NewString()

	sentinel := context.Canceled // any distinguishable error will do

	err := s.WithTx(ctx, func(q *db.Queries) error {
		if _, err := q.CreateProject(ctx, db.CreateProjectParams{
			Slug: slug, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
		}); err != nil {
			return err
		}

		return sentinel
	})
	if err != sentinel { //nolint:errorlint // testing exact error identity returned by WithTx, not a wrapped chain
		t.Fatalf("WithTx returned %v, want the sentinel unwrapped", err)
	}

	if _, err := s.Q().GetProjectBySlug(ctx, slug); err == nil {
		t.Fatal("project created inside a rolled-back transaction is visible outside it")
	}
}

func TestWithTxCommitsOnSuccess(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	slug := "it-commit-" + uuid.NewString()

	err := s.WithTx(ctx, func(q *db.Queries) error {
		_, err := q.CreateProject(ctx, db.CreateProjectParams{
			Slug: slug, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
		})

		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	if _, err := s.Q().GetProjectBySlug(ctx, slug); err != nil {
		t.Fatalf("project committed inside WithTx is not visible outside it: %v", err)
	}
}

// TestEventAppendOnlyTrigger exercises 0002_control_plane's UPDATE-blocking
// trigger through the Go layer's own connection, not just raw psql.
func TestEventAppendOnlyTrigger(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	taskID, runID := seedTaskAndRun(t, s)

	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	if _, err := s.Q().AppendMirrorEvents(ctx, db.AppendMirrorEventsParams{
		RunID: runID, TaskID: taskID,
		Seqs: []int64{1}, Kinds: []string{"a"}, Actors: []string{"agent"},
		Payloads: [][]byte{[]byte(`{}`)}, Ats: []pgtype.Timestamptz{now},
	}); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	// There is deliberately no generated UpdateEvent query — nothing in
	// this codebase is meant to update an event row, ever. Prove the
	// trigger itself blocks it via a raw connection, bypassing sqlc
	// entirely (the point is what Postgres does, not what Go offers).
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	_, err = pool.Exec(ctx, "UPDATE event SET kind = 'mutated' WHERE run_id = $1", runID)
	if err == nil {
		t.Fatal("UPDATE against event succeeded — append-only trigger did not fire")
	}

	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE against event failed, but not with the append-only trigger's message: %v", err)
	}
}

// TestCanarySecretNeverReachesEvent is development-plan.md §8's own
// prescribed check: "redaction filter applies to every emitted event; test
// it with a canary string." Pushes one literal through every documented
// redaction choke point (internal/redact's package doc: RecordEvent/
// RecordViolation/ApplyTaskTransition and AppendMirror) and asserts it
// never lands in the `event` table.
func TestCanarySecretNeverReachesEvent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	taskID, runID := seedTaskAndRun(t, s)

	const canary = "CANARY-SECRET-do-not-persist-9f3a"

	r := redact.New([]string{canary}, nil)

	if _, err := s.RecordEvent(ctx, r, runID, "test_canary", map[string]any{"note": canary}, nil); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	if _, err := s.RecordViolation(ctx, r, runID, "bash", canary, "runner", nil); err != nil {
		t.Fatalf("RecordViolation: %v", err)
	}

	if _, err := s.ApplyTaskTransition(ctx, r, store.TransitionRequest{
		TaskID: taskID, RunID: &runID, Trigger: "cancel", Actor: "test",
		Payload: map[string]any{"note": canary},
	}); err != nil {
		t.Fatalf("ApplyTaskTransition: %v", err)
	}

	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	if _, err := s.AppendMirror(ctx, r, store.MirrorBatch{
		RunID: runID, TaskID: taskID,
		Events: []store.MirrorEvent{{Seq: 999, Kind: "test", Actor: "agent", Payload: []byte(`{"note":"` + canary + `"}`), At: now.Time}},
	}); err != nil {
		t.Fatalf("AppendMirror: %v", err)
	}

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM event WHERE payload::text LIKE '%' || $1 || '%'", canary).Scan(&count); err != nil {
		t.Fatalf("querying for canary leakage: %v", err)
	}

	if count != 0 {
		t.Fatalf("canary secret found in %d event row(s) — redaction did not apply", count)
	}
}
