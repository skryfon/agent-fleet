package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "agentfleet/internal/store/gen"
)

// The methods below satisfy internal/outbox.Store — a thin, differently-typed
// adapter over the sqlc-generated queries (int32 vs. the generated int64
// LIMIT parameter, a time.Time vs. pgtype.Timestamptz, a plain string vs.
// *string) so internal/outbox does not need to import pgtype or the
// generated db package to depend on this store.

// ClaimBatch claims up to limit unpublished, unfailed, currently-available
// outbox rows via SELECT ... FOR UPDATE SKIP LOCKED, bumping their lease
// (available_at) so a concurrent relay worker — in this process or another
// control-plane instance racing a restart — cannot double-claim them.
func (s *Store) ClaimBatch(ctx context.Context, limit int32) ([]db.Outbox, error) {
	return s.q.ClaimOutboxBatch(ctx, int64(limit))
}

// MarkPublished marks an outbox row delivered. Idempotent: marking an
// already-published row published again is a harmless no-op UPDATE.
func (s *Store) MarkPublished(ctx context.Context, id int64) error {
	return s.q.MarkOutboxPublished(ctx, id)
}

// Reschedule sets a row's next claimable time and records the error that
// caused the retry, without incrementing attempts again (ClaimBatch already
// did that at claim time).
func (s *Store) Reschedule(ctx context.Context, id int64, availableAt time.Time, lastErr string) error {
	return s.q.RescheduleOutbox(ctx, db.RescheduleOutboxParams{
		ID:          id,
		AvailableAt: pgtype.Timestamptz{Time: availableAt, Valid: true},
		LastError:   &lastErr,
	})
}

// Fail poisons a row: sets failed_at, records the terminal error. Never
// auto-retried past this point — internal/reconcile (P8) surfaces it, a
// human decides.
func (s *Store) Fail(ctx context.Context, id int64, lastErr string) error {
	return s.q.FailOutbox(ctx, db.FailOutboxParams{ID: id, LastError: &lastErr})
}
