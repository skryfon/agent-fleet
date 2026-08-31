//go:build integration

package outbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"agentfleet/internal/domain"
	"agentfleet/internal/outbox"
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

// TestClaimBatchNeverDoubleClaims is the real-Postgres proof of the
// guarantee internal/outbox/relay_test.go's fakeStore only simulates: two
// concurrent claimers against the SAME live database — modeling two relay
// workers in one process, or two control-plane processes racing a restart
// (docs/adr/0010's crash-durability story) — must never see the same row.
// SELECT ... FOR UPDATE SKIP LOCKED plus the available_at lease bump
// (0002_control_plane.up.sql) is what this test actually exercises.
func TestClaimBatchNeverDoubleClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const rowCount = 200

	for range rowCount {
		key := uuid.NewString()
		if _, err := s.Q().EnqueueOutbox(ctx, db.EnqueueOutboxParams{
			Topic: "test.topic", Payload: []byte(`{}`), Key: &key,
		}); err != nil {
			t.Fatalf("seeding outbox row: %v", err)
		}
	}

	var (
		mu   sync.Mutex
		seen = make(map[int64]int)
	)

	var wg sync.WaitGroup

	for range 8 { // 8 concurrent claimers against one Postgres connection pool
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				rows, err := s.ClaimBatch(ctx, 5)
				if err != nil {
					t.Errorf("ClaimBatch: %v", err)

					return
				}

				if len(rows) == 0 {
					return
				}

				mu.Lock()
				for _, r := range rows {
					seen[r.ID]++
				}
				mu.Unlock()

				for _, r := range rows {
					if err := s.MarkPublished(ctx, r.ID); err != nil {
						t.Errorf("MarkPublished: %v", err)
					}
				}
			}
		}()
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	for id, count := range seen {
		if count != 1 {
			t.Errorf("outbox row %d claimed %d times, want exactly 1", id, count)
		}
	}

	// Every seeded row for THIS test run must have been claimed exactly
	// once; other concurrent test rows (if any) are irrelevant since IDs
	// are globally unique (bigserial).
	if len(seen) < rowCount {
		t.Errorf("only %d of %d seeded rows were claimed by anyone", len(seen), rowCount)
	}
}

// TestTransitionThenRelayDispatch is the end-to-end P3 proof: a real
// ApplyTaskTransition writes state + event + outbox atomically, and a real
// Relay against the same database claims and dispatches the resulting
// effect. This is the shape internal/api's handlers (P4) and
// cmd/control-plane's wiring (P4) will run continuously.
func TestTransitionThenRelayDispatch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := redact.New(nil, nil)

	suffix := uuid.NewString()

	proj, err := s.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "ob-it-project-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := s.Q().CreateFeature(ctx, db.CreateFeatureParams{
		ProjectID: proj.ID, Slug: "ob-it-feature-" + suffix, State: "OPEN",
	})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := s.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "t", Intent: "i",
		AcceptanceCriteria: []byte(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
		SpecRefs: []byte(`[]`), State: "QUEUED",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	// TrStart on a QUEUED task schedules a run.launch effect
	// (internal/domain/transition.go's taskTable) — this is the transition
	// under test.
	result, err := s.ApplyTaskTransition(ctx, r, store.TransitionRequest{
		TaskID: task.ID, Trigger: domain.TrStart, Actor: "test",
	})
	if err != nil {
		t.Fatalf("ApplyTaskTransition: %v", err)
	}

	if result.To != domain.TaskRunning {
		t.Fatalf("transition landed at %s, want RUNNING", result.To)
	}

	if len(result.OutboxIDs) != 1 {
		t.Fatalf("got %d outbox effects, want 1 (run.launch)", len(result.OutboxIDs))
	}

	relay := outbox.NewRelay(s, outbox.Config{Workers: 1, PollInterval: 10 * time.Millisecond}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dispatched := make(chan outbox.Message, 1)
	relay.Handle("run.launch", func(_ context.Context, m outbox.Message) error {
		dispatched <- m

		return nil
	})

	relayCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	go func() { _ = relay.Run(relayCtx) }()

	select {
	case m := <-dispatched:
		if m.Topic != "run.launch" {
			t.Errorf("dispatched topic = %q, want run.launch", m.Topic)
		}
	case <-relayCtx.Done():
		t.Fatal("relay never dispatched the effect ApplyTaskTransition enqueued")
	}

	cancel()

	// The event this transition wrote must be findable and must be the one
	// state-machine-legal step (QUEUED -> RUNNING), confirming
	// ApplyTaskTransition's event write landed in the same transaction as
	// the state change and the outbox row above.
	events, err := s.Q().ListEventsByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListEventsByTask: %v", err)
	}

	if len(events) != 1 || events[0].Kind != "task_started" {
		t.Fatalf("events for task = %+v, want exactly one task_started event", events)
	}
}

// TestMultipleTaskStartsDoNotCollideOnOutboxKey is the direct regression
// test for a real bug this session caught live: the QUEUED->RUNNING launch
// effect was originally keyed by "launch:{{run_id}}", but no run exists yet
// at that point (the launch effect is what creates one) — every task's
// FIRST launch rendered the identical literal key "launch:" (empty
// substitution) forever, so only the first task to ever start actually got
// a run.launch effect; every subsequent task silently got none
// (ON CONFLICT (key) DO NOTHING against the first task's key,
// outbox_key_uk having no expiry). Fixed by keying effects from the
// transition's own event id (see domain.EffectSpec.KeyReason) instead of a
// template. This test starts three independent tasks and asserts each gets
// its own outbox row.
func TestMultipleTaskStartsDoNotCollideOnOutboxKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := redact.New(nil, nil)

	suffix := uuid.NewString()

	proj, err := s.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "ob-it-multi-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := s.Q().CreateFeature(ctx, db.CreateFeatureParams{
		ProjectID: proj.ID, Slug: "f-" + suffix, State: "OPEN",
	})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	seenOutboxIDs := make(map[int64]bool)

	for i := range 3 {
		task, err := s.Q().InsertTask(ctx, db.InsertTaskParams{
			FeatureID: feat.ID, Lane: "direct", Title: "t", Intent: "i",
			AcceptanceCriteria: []byte(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
			SpecRefs: []byte(`[]`), State: "QUEUED",
		})
		if err != nil {
			t.Fatalf("InsertTask %d: %v", i, err)
		}

		result, err := s.ApplyTaskTransition(ctx, r, store.TransitionRequest{
			TaskID: task.ID, Trigger: domain.TrStart, Actor: "test",
		})
		if err != nil {
			t.Fatalf("ApplyTaskTransition %d: %v", i, err)
		}

		if len(result.OutboxIDs) != 1 {
			t.Fatalf("task %d: got %d outbox effects, want 1 — this is the exact bug: task %d's launch silently collided with an earlier task's key", i, len(result.OutboxIDs), i)
		}

		id := result.OutboxIDs[0]
		if seenOutboxIDs[id] {
			t.Fatalf("task %d: outbox id %d was already used by an earlier task", i, id)
		}

		seenOutboxIDs[id] = true
	}

	if len(seenOutboxIDs) != 3 {
		t.Fatalf("got %d distinct outbox rows for 3 task starts, want 3", len(seenOutboxIDs))
	}
}

// TestApplyTaskTransitionRejectsIllegalTransition proves
// domain.ErrIllegalTransition survives Store.WithTx's passthrough intact,
// so an internal/api handler (P4) can errors.Is against it to render a 409
// rather than a 500 — and that nothing is written when it fires.
func TestApplyTaskTransitionRejectsIllegalTransition(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	r := redact.New(nil, nil)

	suffix := uuid.NewString()

	proj, _ := s.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "ob-it-illegal-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
	})
	feat, _ := s.Q().CreateFeature(ctx, db.CreateFeatureParams{ProjectID: proj.ID, Slug: "f-" + suffix, State: "OPEN"})
	task, _ := s.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "t", Intent: "i",
		AcceptanceCriteria: []byte(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
		SpecRefs: []byte(`[]`), State: "CREATED",
	})

	// CREATED has no (CREATED, TrApproved) row — illegal.
	_, err := s.ApplyTaskTransition(ctx, r, store.TransitionRequest{
		TaskID: task.ID, Trigger: domain.TrApproved, Actor: "test",
	})
	if !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("ApplyTaskTransition error = %v, want errors.Is(_, domain.ErrIllegalTransition)", err)
	}

	got, err := s.Q().GetTaskByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}

	if got.State != "CREATED" {
		t.Fatalf("task state = %s after a rejected transition, want unchanged CREATED", got.State)
	}

	events, err := s.Q().ListEventsByTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListEventsByTask: %v", err)
	}

	if len(events) != 0 {
		t.Fatalf("got %d events after a rejected transition, want 0", len(events))
	}
}
