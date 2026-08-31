-- name: InsertTask :one
-- Used by tasks:ingest (internal/domain/tasksmd, P4) for a task whose
-- external_ref hasn't been seen before on this feature.
INSERT INTO task (feature_id, external_ref, lane, title, intent,
                   acceptance_criteria, touches, depends_on, spec_refs, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: UpsertTaskByExternalRef :one
-- Re-ingesting tasks.md updates an existing task's content in place, keyed
-- by (feature_id, external_ref) — task_feature_external_ref_uk from
-- 0002_control_plane.up.sql. Callers must have already rejected (409) any
-- task whose current state is not CREATED/QUEUED before calling this —
-- rewriting the spec under a running agent is not allowed.
INSERT INTO task (feature_id, external_ref, lane, title, intent,
                   acceptance_criteria, touches, depends_on, spec_refs, state)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (feature_id, external_ref) WHERE external_ref IS NOT NULL
DO UPDATE SET
    lane = EXCLUDED.lane,
    title = EXCLUDED.title,
    intent = EXCLUDED.intent,
    acceptance_criteria = EXCLUDED.acceptance_criteria,
    touches = EXCLUDED.touches,
    depends_on = EXCLUDED.depends_on,
    spec_refs = EXCLUDED.spec_refs,
    updated_at = now()
RETURNING *;

-- name: GetTaskByID :one
SELECT * FROM task WHERE id = $1;

-- name: GetTaskForUpdate :one
-- Locks the row for the duration of the caller's transaction — the first
-- step of internal/store.ApplyTaskTransition (P3), so concurrent triggers
-- against the same task serialize instead of racing.
SELECT * FROM task WHERE id = $1 FOR UPDATE;

-- name: GetTaskByFeatureExternalRef :one
SELECT * FROM task WHERE feature_id = $1 AND external_ref = $2;

-- name: ListTasksByFeature :many
SELECT * FROM task WHERE feature_id = $1 ORDER BY created_at;

-- name: ListTasksByState :many
SELECT * FROM task WHERE state = $1 ORDER BY created_at;

-- name: UpdateTaskState :one
-- The only place task.state ever changes. version is bumped for the SSE/
-- read-path optimistic-concurrency story; the transition's own atomicity
-- comes from the caller wrapping this in the same transaction as the
-- event/outbox inserts (internal/store.WithTx), not from this UPDATE alone.
UPDATE task
SET state = $2, version = version + 1, updated_at = now(),
    next_event_seq = next_event_seq + 1
WHERE id = $1
RETURNING *;
