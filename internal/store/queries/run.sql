-- name: InsertRun :one
-- token_hash is the sha256 of the per-run bearer token; the plaintext is
-- never persisted (development-plan.md §8 Secrets) — internal/supervisor
-- hands the plaintext to the container's env directly.
INSERT INTO run (task_id, parent_run_id, role, model, state, token_hash)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetRunByID :one
SELECT * FROM run WHERE id = $1;

-- name: GetRunForUpdate :one
SELECT * FROM run WHERE id = $1 FOR UPDATE;

-- name: GetActiveRunForTask :one
-- run_active_per_task_uk (0002_control_plane.up.sql) guarantees at most one
-- row matches.
SELECT * FROM run WHERE task_id = $1 AND state IN ('PENDING', 'STARTING', 'RUNNING');

-- name: ListActiveRuns :many
-- The reconciler's (P8) and the supervisor's own startup reap's view of
-- "what should currently have a live container."
SELECT * FROM run WHERE state IN ('PENDING', 'STARTING', 'RUNNING') ORDER BY created_at;

-- name: UpdateRunState :one
UPDATE run
SET state = $2, version = version + 1, updated_at = now(),
    next_event_seq = next_event_seq + 1
WHERE id = $1
RETURNING *;

-- name: SetRunContainerStarted :one
UPDATE run
SET state = $2, container_id = $3, started_at = now(),
    version = version + 1, updated_at = now(), next_event_seq = next_event_seq + 1
WHERE id = $1
RETURNING *;

-- name: SetRunExited :one
UPDATE run
SET state = $2, exit_code = $3, ended_at = now(),
    version = version + 1, updated_at = now(), next_event_seq = next_event_seq + 1
WHERE id = $1
RETURNING *;

-- name: TouchRunHeartbeat :exec
-- POST /v1/runs/{id}/checkpoint (P4) calls this; internal/reconcile's
-- "stale runs" job (P8) reads last_heartbeat_at back out via GetRunByID.
UPDATE run SET last_heartbeat_at = now() WHERE id = $1;

-- name: IncrementRunAttempt :one
UPDATE run SET attempt = attempt + 1, updated_at = now() WHERE id = $1
RETURNING *;
