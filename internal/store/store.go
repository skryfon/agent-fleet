// Package store is the Postgres access layer: sqlc-generated queries
// (internal/store/gen) over the schema in deploy/migrations, no ORM
// (development-plan.md §3, §7). The control plane is the only writer
// (development-plan.md §2) — internal/podman and cmd/supervisor (the one
// process with Podman access, D11) never import this package.
// internal/supervisor DOES import it: it runs inside the control-plane
// process as the run.launch/run.kill outbox handlers and only ever talks to
// the separate cmd/supervisor daemon over HTTP, never to Podman directly.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	db "agentfleet/internal/store/gen"
)

// Store wraps a pgx connection pool and the sqlc-generated Queries. Every
// exported method that changes more than one row across the state/event/
// outbox trio goes through WithTx — that single transaction is docs/adr/0010's
// entire atomicity claim ("a state change and its outbound side effects
// commit in one transaction").
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

// lockTimeout bounds how long a session will wait on a row lock —
// GetTaskForUpdate/GetRunForUpdate deliberately block on FOR UPDATE to
// serialize concurrent transitions, but a live-but-hung caller (a goroutine
// that calls WithTx and never returns, e.g. a bug rather than a crash —
// Postgres's own crash recovery already handles a killed backend fine)
// could otherwise hold that lock indefinitely and stall every other request
// against the same hot task. Flagged in DB review: this was previously
// unset. statementTimeout is a second, broader backstop for any query that
// runs unexpectedly long for other reasons.
const (
	lockTimeout      = 5 * time.Second
	statementTimeout = 30 * time.Second
	// maxConns is a documented starting point, not a load-tested figure —
	// this pool is shared by internal/api's handlers, internal/outbox.Relay's
	// workers (4 by default, each polling independently), and
	// internal/reconcile's sweep. Revisit once cmd/control-plane's actual
	// concurrency is wired up and there is real traffic to size against
	// (flagged in DB review as a tracked gap, not urgent to block M2).
	maxConns = 20
)

// Open connects to Postgres and returns a Store backed by a connection
// pool. dsn is a standard postgres:// URL (DATABASE_URL).
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parsing DATABASE_URL: %w", err)
	}

	cfg.MaxConns = maxConns
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, fmt.Sprintf(
			"SET lock_timeout = '%s'; SET statement_timeout = '%s'",
			lockTimeout, statementTimeout,
		))

		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connecting: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("store: ping: %w", err)
	}

	return &Store{pool: pool, q: db.New(pool)}, nil
}

// Close releases the pool. Safe to call once, at process shutdown.
func (s *Store) Close() {
	s.pool.Close()
}

// Ping reports whether the pool can reach Postgres — the /readyz handler's
// only dependency.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Q exposes the raw generated queries for read-only call sites (internal/api
// handlers that only SELECT) that don't need a transaction. Any call site
// that writes to more than one of {task/run, event, outbox} in one logical
// operation must use WithTx instead — see internal/store/transition.go's
// ApplyTaskTransition for the pattern.
func (s *Store) Q() *db.Queries {
	return s.q
}

// WithTx runs fn inside a single Postgres transaction, passing it a
// *db.Queries bound to that transaction. If fn returns a non-nil error, the
// transaction is rolled back and that error is returned unwrapped (so
// callers can errors.Is/As against domain sentinels like
// domain.ErrIllegalTransition). If fn succeeds, the transaction commits;
// a commit failure is returned wrapped.
//
// This is the ONE place a state change, its event row, and its outbox rows
// are allowed to be written together — every transition handler
// (internal/store/transition.go) calls it exactly once. That is what makes
// "killing the control plane mid-run loses nothing" (development-plan.md's
// M2 done-condition) true: either all of it lands, or none of it does.
func (s *Store) WithTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}

	committed := false

	defer func() {
		if !committed {
			// Best-effort: ctx may already be cancelled at this point, and
			// a rollback on an already-closed connection is not itself an
			// error worth surfacing — the transaction is gone either way.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	qtx := s.q.WithTx(tx)

	if err := fn(qtx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}

	committed = true

	return nil
}
