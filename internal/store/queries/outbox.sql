-- name: EnqueueOutbox :one
-- key renders from domain.EffectSpec.RenderKey — a transition retried after
-- a crash re-derives the identical key, so this INSERT is a no-op on retry.
-- The ON CONFLICT target MUST repeat the partial index's WHERE clause
-- (outbox_key_uk is `ON outbox (key) WHERE key IS NOT NULL`) or Postgres
-- refuses the statement with "no unique or exclusion constraint matching
-- the ON CONFLICT specification" — verified live against
-- 0002_control_plane.up.sql while writing that migration.
INSERT INTO outbox (topic, payload, key)
VALUES ($1, $2, $3)
ON CONFLICT (key) WHERE key IS NOT NULL DO NOTHING
RETURNING *;

-- name: ClaimOutboxBatch :many
-- SKIP LOCKED + the available_at lease bump means a second relay process
-- (or a restart racing the old one's in-flight work) never double-claims a
-- row — internal/outbox.Relay (P3) is the only caller.
UPDATE outbox
SET attempts = attempts + 1, available_at = now() + interval '30 seconds'
WHERE id IN (
    SELECT id FROM outbox
    WHERE published_at IS NULL AND failed_at IS NULL AND available_at <= now()
    ORDER BY available_at, id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkOutboxPublished :exec
UPDATE outbox SET published_at = now() WHERE id = $1;

-- name: RescheduleOutbox :exec
UPDATE outbox SET available_at = $2, last_error = $3 WHERE id = $1;

-- name: FailOutbox :exec
-- Poison: attempts exhausted (internal/outbox.Relay's MaxAttempts). Never
-- auto-retried past this point — internal/reconcile (P8) surfaces it, a
-- human decides.
UPDATE outbox SET failed_at = now(), last_error = $2 WHERE id = $1;

-- name: CountPoisonedOutbox :one
SELECT count(*) FROM outbox WHERE failed_at IS NOT NULL;

-- name: CountStalledOutbox :one
SELECT count(*) FROM outbox
WHERE published_at IS NULL AND failed_at IS NULL AND created_at < $1;
