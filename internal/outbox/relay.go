// Package outbox implements the dispatcher half of the transactional
// outbox pattern (docs/adr/0010): internal/store writes outbox rows inside
// the same transaction as a state change; Relay is the separate process
// loop that claims and dispatches those rows asynchronously, with retry and
// backoff, so a slow or momentarily-down downstream effect (a container
// launch, a Zulip message) never blocks — or loses — the state change it
// followed from.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"golang.org/x/sync/errgroup"

	db "agentfleet/internal/store/gen"
)

// Message is one claimed outbox row, handed to the Handler registered for
// its Topic.
type Message struct {
	ID       int64
	Topic    string
	Key      string // empty if the row had no key (not every effect needs enqueue-idempotency)
	Payload  []byte
	Attempts int32
}

// Handler processes one Message. It MUST be idempotent on Key — a handler
// can be called more than once for logically the same effect (a retry after
// a failure, or a claim that succeeded but whose ack was lost to a crash
// between claim and MarkOutboxPublished) and must produce the same
// observable outcome either way. Returning nil marks the row published;
// returning ErrPoison fails it immediately, skipping retries; any other
// error schedules a backoff retry.
type Handler func(ctx context.Context, m Message) error

// ErrPoison, returned from a Handler, marks a message as unrecoverable
// immediately rather than retrying it MaxAttempts times first — use it for
// errors retrying cannot fix (e.g. the payload itself is malformed).
var ErrPoison = errors.New("outbox: unrecoverable, not retrying")

// Store is the subset of *store.Store the relay needs. Kept as an
// interface (rather than importing agentfleet/internal/store directly) so
// relay_test.go can exercise the claim/backoff/poison logic against a fake
// without a real Postgres, while the integration tests still cover the real
// claim query's SKIP LOCKED behavior.
type Store interface {
	ClaimBatch(ctx context.Context, limit int32) ([]db.Outbox, error)
	MarkPublished(ctx context.Context, id int64) error
	Reschedule(ctx context.Context, id int64, availableAt time.Time, lastErr string) error
	Fail(ctx context.Context, id int64, lastErr string) error
}

// Config tunes the relay's claim batching, concurrency, and retry/backoff
// policy. Every field has a documented default via NewRelay — a caller
// constructing Config directly should still set every field it cares about,
// since Config{} has no useful defaults of its own (zero workers claims
// nothing).
type Config struct {
	Workers      int
	BatchSize    int32
	PollInterval time.Duration
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	MaxAttempts  int32
}

// DefaultConfig matches the values docs/adr/0010's ~30-minute retry budget
// before poisoning (MaxAttempts * an exponential ramp capped at MaxBackoff)
// was sized against.
func DefaultConfig() Config {
	return Config{
		Workers:      4,
		BatchSize:    32,
		PollInterval: 250 * time.Millisecond,
		BaseBackoff:  time.Second,
		MaxBackoff:   5 * time.Minute,
		MaxAttempts:  12,
	}
}

// Relay claims and dispatches outbox rows. The zero value is not usable;
// construct with NewRelay.
type Relay struct {
	store    Store
	cfg      Config
	handlers map[string]Handler
	log      *slog.Logger
}

// NewRelay constructs a Relay. Zero-value Config fields fall back to
// DefaultConfig's corresponding value, so a caller can override just the
// fields it cares about.
func NewRelay(store Store, cfg Config, log *slog.Logger) *Relay {
	def := DefaultConfig()
	if cfg.Workers <= 0 {
		cfg.Workers = def.Workers
	}

	if cfg.BatchSize <= 0 {
		cfg.BatchSize = def.BatchSize
	}

	if cfg.PollInterval <= 0 {
		cfg.PollInterval = def.PollInterval
	}

	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = def.BaseBackoff
	}

	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = def.MaxBackoff
	}

	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = def.MaxAttempts
	}

	if log == nil {
		log = slog.Default()
	}

	return &Relay{store: store, cfg: cfg, handlers: make(map[string]Handler), log: log}
}

// Handle registers h for topic. Call before Run; registering after Run has
// started is not safe for concurrent use.
func (r *Relay) Handle(topic string, h Handler) {
	r.handlers[topic] = h
}

// Run starts cfg.Workers claim/dispatch loops and blocks until ctx is
// cancelled, then returns once every in-flight message has been handled
// (a worker never abandons a message it already claimed mid-handler; it
// finishes the current batch and exits).
func (r *Relay) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	for range r.cfg.Workers {
		g.Go(func() error {
			return r.workerLoop(gctx)
		})
	}

	return g.Wait()
}

func (r *Relay) workerLoop(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.claimAndDispatchBatch(ctx); err != nil {
				r.log.ErrorContext(ctx, "outbox: claim batch failed", "error", err)
			}
		}
	}
}

func (r *Relay) claimAndDispatchBatch(ctx context.Context) error {
	rows, err := r.store.ClaimBatch(ctx, int32(r.cfg.BatchSize))
	if err != nil {
		return fmt.Errorf("outbox: claiming batch: %w", err)
	}

	for _, row := range rows {
		r.dispatchOne(ctx, row)
	}

	return nil
}

func (r *Relay) dispatchOne(ctx context.Context, row db.Outbox) {
	key := ""
	if row.Key != nil {
		key = *row.Key
	}

	msg := Message{ID: row.ID, Topic: row.Topic, Key: key, Payload: row.Payload, Attempts: row.Attempts}

	handler, ok := r.handlers[row.Topic]
	if !ok {
		// A topic with no registered handler is a configuration bug, not a
		// transient failure — poison it immediately rather than retrying
		// forever against a handler that will never exist this process
		// run. internal/reconcile surfaces poisoned rows for a human.
		r.log.ErrorContext(ctx, "outbox: no handler registered for topic", "topic", row.Topic, "id", row.ID)
		r.fail(ctx, row.ID, fmt.Sprintf("no handler registered for topic %q", row.Topic))

		return
	}

	err := handler(ctx, msg)

	switch {
	case err == nil:
		if mErr := r.store.MarkPublished(ctx, row.ID); mErr != nil {
			r.log.ErrorContext(ctx, "outbox: marking published failed", "id", row.ID, "error", mErr)
		}
	case errors.Is(err, ErrPoison):
		r.fail(ctx, row.ID, err.Error())
	case row.Attempts >= r.cfg.MaxAttempts:
		r.log.ErrorContext(ctx, "outbox: attempts exhausted, poisoning", "id", row.ID, "topic", row.Topic, "attempts", row.Attempts)
		r.fail(ctx, row.ID, err.Error())
	default:
		delay := backoff(r.cfg.BaseBackoff, r.cfg.MaxBackoff, row.Attempts)
		if rErr := r.store.Reschedule(ctx, row.ID, time.Now().Add(delay), err.Error()); rErr != nil {
			r.log.ErrorContext(ctx, "outbox: reschedule failed", "id", row.ID, "error", rErr)
		}
	}
}

func (r *Relay) fail(ctx context.Context, id int64, reason string) {
	if err := r.store.Fail(ctx, id, reason); err != nil {
		r.log.ErrorContext(ctx, "outbox: marking failed (poison) failed", "id", id, "error", err)
	}
}

// backoff is exponential with ±20% jitter, capped at maxBackoff. Jitter
// prevents every row that failed in the same batch from retrying in
// lockstep and re-contending the same claim query at the same instant.
func backoff(base, maxBackoff time.Duration, attempts int32) time.Duration {
	d := base

	for range attempts {
		d *= 2
		if d >= maxBackoff {
			d = maxBackoff

			break
		}
	}

	jitter := float64(d) * (0.8 + 0.4*rand.Float64()) //nolint:gosec // jitter timing, not a security-sensitive value

	return time.Duration(jitter)
}
