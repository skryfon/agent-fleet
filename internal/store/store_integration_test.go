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

// insertRunningRunForTask stands in for what internal/supervisor.RunLaunch
// would do once the run.launch effect ApplySpawn's TrStart schedules is
// actually processed by the outbox relay — this package's own tests never
// run that relay, so a two-level spawn (a grandchild spawned FROM a
// spawned child) needs the child's run row inserted directly, the same way
// seedTaskAndRun seeds the root's.
func insertRunningRunForTask(t *testing.T, s *store.Store, taskID uuid.UUID) uuid.UUID {
	t.Helper()

	run, err := s.Q().InsertRun(context.Background(), db.InsertRunParams{
		TaskID: taskID, Role: "implementer", Model: "test-model", State: "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	return run.ID
}

// TestApplySpawnCreatesChildTaskAtNextDepth is ApplySpawn's own basic
// contract check: a spawned child lands in the SAME feature, one depth
// level deeper than its parent, already QUEUED->RUNNING (spawn_worker
// hands the orchestrator a worker that's already going, not one it must
// separately POST /start for), and task.parent_run_id is the spawning run
// — CancelSubtree's walk key. (A concrete run row for the child is
// internal/supervisor.RunLaunch's job, processing the run.launch effect
// this spawn schedules — not ApplySpawn's; see
// internal/supervisor/handlers_integration_test.go for that half.)
func TestApplySpawnCreatesChildTaskAtNextDepth(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := redact.New(nil, nil)
	parentTaskID, parentRunID := seedTaskAndRun(t, s)

	result, err := s.ApplySpawn(ctx, r, store.SpawnRequest{
		ParentTaskID: parentTaskID, ParentRunID: parentRunID,
		Title: "child task", Intent: "do the sub-work",
		Actor: "test",
	})
	if err != nil {
		t.Fatalf("ApplySpawn: %v", err)
	}

	if result.Child.Depth != 1 {
		t.Fatalf("child.Depth = %d, want 1", result.Child.Depth)
	}

	parentTask, err := s.Q().GetTaskByID(ctx, parentTaskID)
	if err != nil {
		t.Fatalf("GetTaskByID(parent): %v", err)
	}

	if result.Child.FeatureID != parentTask.FeatureID {
		t.Fatalf("child.FeatureID = %s, want %s (same feature as parent)", result.Child.FeatureID, parentTask.FeatureID)
	}

	if result.Started.To != "RUNNING" {
		t.Fatalf("child task ended at %s, want RUNNING", result.Started.To)
	}

	if !result.Child.ParentRunID.Valid || result.Child.ParentRunID.Bytes != parentRunID {
		t.Fatalf("child task.parent_run_id = %+v, want %s", result.Child.ParentRunID, parentRunID)
	}

	if len(result.Started.OutboxIDs) != 1 {
		t.Fatalf("TrStart enqueued %d outbox rows, want 1 (run.launch)", len(result.Started.OutboxIDs))
	}
}

// TestCancelSubtreeCancelsEveryDescendant is half of M5's own done-when
// (development-plan.md §7: "cancelling the parent kills the subtree"): a
// grandchild spawned by a spawned child must be cancelled too, and each
// cancelled task's own run.kill effect must be enqueued — not just the
// direct child's.
func TestCancelSubtreeCancelsEveryDescendant(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := redact.New(nil, nil)
	rootTaskID, rootRunID := seedTaskAndRun(t, s)

	childResult, err := s.ApplySpawn(ctx, r, store.SpawnRequest{
		ParentTaskID: rootTaskID, ParentRunID: rootRunID,
		Title: "child", Intent: "child work", Actor: "test",
	})
	if err != nil {
		t.Fatalf("ApplySpawn(child): %v", err)
	}

	childRunID := insertRunningRunForTask(t, s, childResult.Child.ID)

	grandchildResult, err := s.ApplySpawn(ctx, r, store.SpawnRequest{
		ParentTaskID: childResult.Child.ID, ParentRunID: childRunID,
		Title: "grandchild", Intent: "grandchild work", Actor: "test",
	})
	if err != nil {
		t.Fatalf("ApplySpawn(grandchild): %v", err)
	}

	if grandchildResult.Child.Depth != 2 {
		t.Fatalf("grandchild.Depth = %d, want 2", grandchildResult.Child.Depth)
	}

	results, err := s.CancelSubtree(ctx, r, rootTaskID, "test:cancel")
	if err != nil {
		t.Fatalf("CancelSubtree: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("CancelSubtree returned %d transitions, want 3 (root, child, grandchild)", len(results))
	}

	for _, id := range []uuid.UUID{rootTaskID, childResult.Child.ID, grandchildResult.Child.ID} {
		task, err := s.Q().GetTaskByID(ctx, id)
		if err != nil {
			t.Fatalf("GetTaskByID(%s): %v", id, err)
		}

		if task.State != "CANCELLED" {
			t.Fatalf("task %s state = %s, want CANCELLED", id, task.State)
		}
	}

	for _, res := range results {
		if len(res.OutboxIDs) != 1 {
			t.Fatalf("transition for a cancelled task enqueued %d outbox rows, want 1 (run.kill)", len(res.OutboxIDs))
		}
	}
}

// TestCancelSubtreeSkipsAlreadyTerminalTasks proves a subtree containing a
// task that's already terminal (DONE/FAILED/CANCELLED/PARKED) is not itself
// an error — ListActiveSubtreeTaskIDs excludes terminal tasks, so
// CancelSubtree simply has fewer tasks to cancel, rather than failing on an
// illegal transition. REVIEW is deliberately NOT used for "already
// finished" here: it is not terminal (domain.TaskReview still accepts
// TrCancel), so a REVIEW child is correctly INCLUDED in a subtree cancel —
// PARKED is the terminal state this test actually needs.
func TestCancelSubtreeSkipsAlreadyTerminalTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := redact.New(nil, nil)
	rootTaskID, rootRunID := seedTaskAndRun(t, s)

	childResult, err := s.ApplySpawn(ctx, r, store.SpawnRequest{
		ParentTaskID: rootTaskID, ParentRunID: rootRunID,
		Title: "child", Intent: "child work", Actor: "test",
	})
	if err != nil {
		t.Fatalf("ApplySpawn: %v", err)
	}

	// Park the child on its own, independently of the parent, before the
	// parent is ever cancelled — PARKED is terminal, so this must be the
	// task ListActiveSubtreeTaskIDs excludes.
	if _, err := s.ApplyTaskTransition(ctx, r, store.TransitionRequest{
		TaskID: childResult.Child.ID, Trigger: "park", Actor: "test",
	}); err != nil {
		t.Fatalf("ApplyTaskTransition(child, park): %v", err)
	}

	results, err := s.CancelSubtree(ctx, r, rootTaskID, "test:cancel")
	if err != nil {
		t.Fatalf("CancelSubtree: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("CancelSubtree returned %d transitions, want 1 (root only — child already PARKED)", len(results))
	}

	child, err := s.Q().GetTaskByID(ctx, childResult.Child.ID)
	if err != nil {
		t.Fatalf("GetTaskByID(child): %v", err)
	}

	if child.State != "PARKED" {
		t.Fatalf("child state = %s, want PARKED (untouched by the parent's cancel)", child.State)
	}
}
