//go:build integration

// Integration tests require a real Postgres at DATABASE_URL, migrated up to
// deploy/migrations' latest revision — see `make test-integration`.
package questions_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"agentfleet/internal/questions"
	"agentfleet/internal/redact"
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

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeNotifier struct{ calls int }

func (f *fakeNotifier) Notify(_ context.Context, _ zulip.NotifyRequest) error {
	f.calls++

	return nil
}

// backdateAskedAt directly rewrites question.asked_at — the sweeper's own
// age math reads this column, and no sqlc query exists (deliberately;
// nothing else should ever need to set it) for what only a test needs, so
// a raw connection is the right tool here, not a new production query.
func backdateAskedAt(t *testing.T, dsn string, questionID uuid.UUID, age time.Duration) {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(context.Background(),
		"UPDATE question SET asked_at = now() - $2::interval WHERE id = $1", questionID, age.String(),
	); err != nil {
		t.Fatalf("backdating asked_at: %v", err)
	}
}

// seedOpenQuestion creates a project/feature/task/run/question chain, backdates
// asked_at by age, and returns the question and task ids.
func seedOpenQuestion(t *testing.T, s *store.Store, age time.Duration) (questionID, taskID uuid.UUID) {
	t.Helper()

	ctx := context.Background()
	suffix := uuid.NewString()

	proj, err := s.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "sweep-it-project-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := s.Q().CreateFeature(ctx, db.CreateFeatureParams{ProjectID: proj.ID, Slug: "sweep-it-feature-" + suffix, State: "OPEN"})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := s.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "sweep test task", Intent: "verify the timeout ladder",
		AcceptanceCriteria: json.RawMessage(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
		SpecRefs: json.RawMessage(`[]`), State: "BLOCKED_ON_HUMAN",
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
		Kind: "free_text", Body: "still waiting?", Options: []byte(`[]`), State: "OPEN",
	})
	if err != nil {
		t.Fatalf("CreateQuestion: %v", err)
	}

	backdateAskedAt(t, os.Getenv("DATABASE_URL"), q.ID, age)

	return q.ID, task.ID
}

func TestSweepFiresNudgeThenEscalateThenPark(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	notifier := &fakeNotifier{}
	sweeper := &questions.Sweeper{
		Store: s, Redact: redact.New(nil, nil), Zulip: notifier, Log: testLogger(), Config: questions.DefaultConfig(),
	}

	// Nudge: 5h old, past the 4h rung.
	nudgeQID, _ := seedOpenQuestion(t, s, 5*time.Hour)
	if err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	q, err := s.Q().GetQuestionByID(ctx, nudgeQID)
	if err != nil {
		t.Fatalf("GetQuestionByID: %v", err)
	}

	if !q.NudgedAt.Valid {
		t.Fatal("question 5h old was not nudged")
	}

	if q.State != "OPEN" {
		t.Fatalf("state = %s, want still OPEN after a nudge", q.State)
	}

	// Escalate: 25h old.
	escQID, _ := seedOpenQuestion(t, s, 25*time.Hour)
	if err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	q, err = s.Q().GetQuestionByID(ctx, escQID)
	if err != nil {
		t.Fatalf("GetQuestionByID: %v", err)
	}

	if !q.EscalatedAt.Valid {
		t.Fatal("question 25h old was not escalated")
	}

	// Park: 73h old — question TIMED_OUT, task PARKED.
	parkQID, parkTaskID := seedOpenQuestion(t, s, 73*time.Hour)
	if err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	q, err = s.Q().GetQuestionByID(ctx, parkQID)
	if err != nil {
		t.Fatalf("GetQuestionByID: %v", err)
	}

	if q.State != "TIMED_OUT" {
		t.Fatalf("state = %s, want TIMED_OUT after 73h", q.State)
	}

	task, err := s.Q().GetTaskByID(ctx, parkTaskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}

	if task.State != "PARKED" {
		t.Fatalf("task state = %s, want PARKED after 73h", task.State)
	}

	if notifier.calls < 2 {
		t.Fatalf("notifier.calls = %d, want at least 2 (one nudge, one escalate)", notifier.calls)
	}
}

// TestSweepDoesNotRepeatAFiredRung proves the mark (nudged_at/escalated_at)
// actually suppresses a re-fire on the next sweep — the whole reason those
// columns exist.
func TestSweepDoesNotRepeatAFiredRung(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	notifier := &fakeNotifier{}
	sweeper := &questions.Sweeper{
		Store: s, Redact: redact.New(nil, nil), Zulip: notifier, Log: testLogger(), Config: questions.DefaultConfig(),
	}

	qID, _ := seedOpenQuestion(t, s, 5*time.Hour)

	if err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatalf("first SweepOnce: %v", err)
	}

	if err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatalf("second SweepOnce: %v", err)
	}

	q, err := s.Q().GetQuestionByID(ctx, qID)
	if err != nil {
		t.Fatalf("GetQuestionByID: %v", err)
	}

	if !q.NudgedAt.Valid {
		t.Fatal("question was never nudged")
	}

	if notifier.calls != 1 {
		t.Fatalf("notifier.calls = %d, want exactly 1 (second sweep must not re-nudge)", notifier.calls)
	}
}
