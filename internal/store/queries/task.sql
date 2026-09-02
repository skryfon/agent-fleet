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

-- name: IncrementTaskAttempt :one
-- Caught in code review: a task's retry cap must survive across the
-- several run rows a retried task goes through (each retry gets a BRAND
-- NEW run, whose own run.attempt starts back at 0) — see
-- 0002_control_plane.up.sql's sourcing comment on task.attempt.
-- internal/store.ApplyRunExit calls this under the same GetTaskForUpdate
-- lock as its own UpdateTaskState call, in the same transaction.
UPDATE task SET attempt = attempt + 1, updated_at = now() WHERE id = $1
RETURNING *;

-- name: InsertChildTask :one
-- M5's spawn_worker (internal/store.ApplySpawn): a child task in the SAME
-- feature as its spawning run's own task, one hop deeper. lane is always
-- 'direct' — a spawned worker never goes through the Spec Kit ingestion
-- lane (D5/D6 govern planning, not fan-out).
INSERT INTO task (feature_id, lane, title, intent, acceptance_criteria,
                   state, parent_run_id, depth, role)
VALUES ($1, 'direct', $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: CountActiveChildTasksForRun :one
-- The direct fan-out width internal/fanout.Check's MaxChildrenPerRun caps —
-- how many still-active tasks the given run has already spawned.
SELECT count(*) FROM task
WHERE parent_run_id = $1
  AND state NOT IN ('DONE', 'FAILED', 'CANCELLED', 'PARKED');

-- name: CountActiveSubtreeTasks :one
-- The aggregate width internal/fanout.Check's MaxActiveSubtree caps: every
-- still-active task anywhere under rootTaskID's own lineage (itself
-- included), walked transitively through parent_run_id -> run.task_id.
-- Mirrors ListActiveSubtreeTaskIDs below; kept as a separate :one query
-- (COUNT, not the row set) since spawn_worker's hot path only needs the
-- number, not the ids.
WITH RECURSIVE sub AS (
    SELECT id FROM task WHERE id = @root_task_id::uuid
    UNION
    SELECT t.id FROM task t
      JOIN run r ON t.parent_run_id = r.id
      JOIN sub s ON r.task_id = s.id
)
SELECT count(*) FROM task
WHERE id IN (SELECT id FROM sub)
  AND state NOT IN ('DONE', 'FAILED', 'CANCELLED', 'PARKED');

-- name: ListActiveSubtreeTaskIDs :many
-- internal/store.CancelSubtree's own view of "everything under this task":
-- the task itself, plus every task spawned (transitively, via
-- parent_run_id -> run.task_id) by any run of it — excluding tasks already
-- in a terminal state, so cancelling a subtree never attempts an illegal
-- transition out of DONE/FAILED/CANCELLED/PARKED.
WITH RECURSIVE sub AS (
    SELECT id FROM task WHERE id = @root_task_id::uuid
    UNION
    SELECT t.id FROM task t
      JOIN run r ON t.parent_run_id = r.id
      JOIN sub s ON r.task_id = s.id
)
SELECT id FROM task
WHERE id IN (SELECT id FROM sub)
  AND state NOT IN ('DONE', 'FAILED', 'CANCELLED', 'PARKED');

-- name: ListActiveTasksByFeature :many
-- The M5 feature-scope budget breach reaction (internal/api/usage.go): every
-- active task in the feature, each cancelled as its own subtree root so a
-- runaway worker's siblings stop burning the feature's remaining budget too.
SELECT * FROM task
WHERE feature_id = $1
  AND state NOT IN ('DONE', 'FAILED', 'CANCELLED', 'PARKED')
ORDER BY created_at;
