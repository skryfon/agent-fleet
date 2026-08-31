package outbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agentfleet/internal/outbox"
	db "agentfleet/internal/store/gen"
)

// fakeStore is an in-memory stand-in for internal/store's outbox methods —
// exercises Relay's claim/backoff/poison logic without a real Postgres. The
// SKIP LOCKED concurrent-claim behavior itself is real-Postgres-only and
// covered by store_integration_test.go (P3's integration suite), not here.
type fakeStore struct {
	mu        sync.Mutex
	rows      map[int64]*db.Outbox
	inFlight  map[int64]bool         // models Postgres's available_at lease bump: a claimed-but-not-yet-acked row is not reclaimable
	claimHook func(rows []db.Outbox) // observes what a claim call returns, for assertions
}

func newFakeStore(rows ...db.Outbox) *fakeStore {
	m := make(map[int64]*db.Outbox, len(rows))
	for i := range rows {
		r := rows[i]
		m[r.ID] = &r
	}

	return &fakeStore{rows: m, inFlight: make(map[int64]bool, len(rows))}
}

func (f *fakeStore) ClaimBatch(_ context.Context, limit int32) ([]db.Outbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var claimed []db.Outbox

	for _, r := range f.rows {
		if len(claimed) >= int(limit) {
			break
		}

		if !r.PublishedAt.Valid && !r.FailedAt.Valid && !f.inFlight[r.ID] {
			r.Attempts++
			f.inFlight[r.ID] = true
			claimed = append(claimed, *r)
		}
	}

	if f.claimHook != nil {
		f.claimHook(claimed)
	}

	return claimed, nil
}

func (f *fakeStore) MarkPublished(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[id].PublishedAt.Valid = true
	delete(f.inFlight, id)

	return nil
}

func (f *fakeStore) Reschedule(_ context.Context, id int64, _ time.Time, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[id].LastError = &lastErr
	delete(f.inFlight, id) // reclaimable again — real Postgres reflects this via the bumped available_at elapsing

	return nil
}

func (f *fakeStore) Fail(_ context.Context, id int64, lastErr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[id].FailedAt.Valid = true
	f.rows[id].LastError = &lastErr
	delete(f.inFlight, id)

	return nil
}

func (f *fakeStore) attempts(id int64) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.rows[id].Attempts
}

func (f *fakeStore) isPublished(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.rows[id].PublishedAt.Valid
}

func (f *fakeStore) isFailed(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.rows[id].FailedAt.Valid
}

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRelayHandlerSuccessMarksPublished(t *testing.T) {
	fs := newFakeStore(db.Outbox{ID: 1, Topic: "run.launch"})
	r := outbox.NewRelay(fs, outbox.Config{Workers: 1, PollInterval: time.Millisecond}, silentLogger())

	var called int32

	r.Handle("run.launch", func(_ context.Context, m outbox.Message) error {
		called++
		if m.ID != 1 {
			t.Errorf("handler got id=%d, want 1", m.ID)
		}

		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = r.Run(ctx)

	if called == 0 {
		t.Fatal("handler was never called")
	}

	if !fs.isPublished(1) {
		t.Fatal("row was not marked published")
	}
}

func TestRelayRetriesThenSucceeds(t *testing.T) {
	fs := newFakeStore(db.Outbox{ID: 1, Topic: "run.launch"})
	r := outbox.NewRelay(fs, outbox.Config{
		Workers: 1, PollInterval: time.Millisecond,
		BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, MaxAttempts: 10,
	}, silentLogger())

	var mu sync.Mutex

	calls := 0

	r.Handle("run.launch", func(_ context.Context, _ outbox.Message) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		if n < 3 {
			return errors.New("transient failure")
		}

		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})

	go func() {
		for !fs.isPublished(1) {
			time.Sleep(5 * time.Millisecond)

			select {
			case <-ctx.Done():
				close(done)

				return
			default:
			}
		}

		close(done)
	}()

	_ = r.Run(ctx)
	<-done

	if !fs.isPublished(1) {
		t.Fatalf("row never published after retries; attempts=%d", fs.attempts(1))
	}

	mu.Lock()
	defer mu.Unlock()

	if calls < 3 {
		t.Fatalf("handler called %d times, want at least 3 (2 failures + 1 success)", calls)
	}
}

func TestRelayPoisonsAfterMaxAttempts(t *testing.T) {
	fs := newFakeStore(db.Outbox{ID: 1, Topic: "run.launch"})
	r := outbox.NewRelay(fs, outbox.Config{
		Workers: 1, PollInterval: time.Millisecond,
		BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, MaxAttempts: 3,
	}, silentLogger())

	r.Handle("run.launch", func(_ context.Context, _ outbox.Message) error {
		return errors.New("permanent-looking failure")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_ = r.Run(ctx)

	if !fs.isFailed(1) {
		t.Fatalf("row was never poisoned after exceeding MaxAttempts; attempts=%d", fs.attempts(1))
	}

	if fs.isPublished(1) {
		t.Fatal("poisoned row must never be marked published")
	}
}

func TestRelayErrPoisonSkipsRetries(t *testing.T) {
	fs := newFakeStore(db.Outbox{ID: 1, Topic: "run.launch"})
	r := outbox.NewRelay(fs, outbox.Config{
		Workers: 1, PollInterval: time.Millisecond, MaxAttempts: 100, // would never poison via attempts alone in this window
	}, silentLogger())

	var calls int32

	r.Handle("run.launch", func(_ context.Context, _ outbox.Message) error {
		calls++

		return outbox.ErrPoison
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = r.Run(ctx)

	if !fs.isFailed(1) {
		t.Fatal("ErrPoison did not poison the row")
	}

	if calls != 1 {
		t.Fatalf("handler called %d times, want exactly 1 (ErrPoison must not retry)", calls)
	}
}

func TestRelayUnknownTopicPoisons(t *testing.T) {
	fs := newFakeStore(db.Outbox{ID: 1, Topic: "no.such.handler"})
	r := outbox.NewRelay(fs, outbox.Config{Workers: 1, PollInterval: time.Millisecond}, silentLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = r.Run(ctx)

	if !fs.isFailed(1) {
		t.Fatal("a message on a topic with no registered handler must poison, not loop forever")
	}
}

func TestRelayEachKeyObservedExactlyOnce(t *testing.T) {
	// Simulates the two-relay-instance concurrency safety the real
	// SKIP LOCKED claim query provides: this test's fakeStore.ClaimBatch is
	// itself mutex-serialized (as if it were one Postgres row lock), so two
	// Relay workers racing to claim never see the same row — proving the
	// HANDLER side of the contract (each Key observed exactly once) given
	// that guarantee. The claim guarantee itself is verified against real
	// Postgres in store_integration_test.go.
	rows := make([]db.Outbox, 50)
	for i := range rows {
		rows[i] = db.Outbox{ID: int64(i + 1), Topic: "t"}
	}

	fs := newFakeStore(rows...)
	r := outbox.NewRelay(fs, outbox.Config{Workers: 8, PollInterval: time.Millisecond, BatchSize: 4}, silentLogger())

	var mu sync.Mutex

	seen := make(map[int64]int)

	r.Handle("t", func(_ context.Context, m outbox.Message) error {
		mu.Lock()
		seen[m.ID]++
		mu.Unlock()

		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_ = r.Run(ctx)

	mu.Lock()
	defer mu.Unlock()

	for id, count := range seen {
		if count != 1 {
			t.Errorf("row %d observed %d times, want exactly 1", id, count)
		}
	}

	if len(seen) != len(rows) {
		t.Errorf("observed %d distinct rows, want %d", len(seen), len(rows))
	}
}
