-- name: InsertRun :one
-- token_hash is the sha256 of the per-run bearer token; the plaintext is
-- never persisted (development-plan.md §8 Secrets) — internal/supervisor
-- hands the plaintext to the container's env directly.
INSERT INTO run (task_id, parent_run_id, role, model, state, token_hash)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: InsertRunWithID :one
-- internal/supervisor's run.launch handler (P5) needs the run's id BEFORE
-- insert, to derive its bearer token deterministically
-- (HMAC-SHA256(SUPERVISOR_SECRET, run_id) — see handlers.go's runToken) so a
-- redelivered launch re-derives the identical token instead of needing the
-- plaintext persisted anywhere. Same columns as InsertRun plus an explicit
-- id in place of the table's gen_random_uuid() default.
INSERT INTO run (id, task_id, parent_run_id, role, model, state, token_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7)
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

-- name: SetRunDshSessionID :exec
-- POST /v1/runs/{id}/checkpoint's own M3 payload field: af-ask-human's
-- checkpoint-and-exit call reports the dsh session id af-resume will later
-- pass to agents.resume() (internal/supervisor.RunLaunch reads it back out
-- via GetRunByID on the resume launch path).
UPDATE run SET dsh_session_id = $2 WHERE id = $1;

-- name: IncrementRunAttempt :one
UPDATE run SET attempt = attempt + 1, updated_at = now() WHERE id = $1
RETURNING *;

-- name: GetLaunchContext :one
-- internal/supervisor's run.launch handler (P5) needs exactly this to build
-- a launch request: the task content for TASK, the project's repo for
-- REPO_URL (repos[1], sqlc/pg arrays are 1-indexed) — every project has
-- exactly one repo before M6's multi-repo manifest work — (M5)
-- t.parent_run_id/t.role, so a spawned child's run row carries its own
-- parent_run_id (CancelSubtree's walk key) and role without a second query
-- — and (M6) the project's slug (per-project GH_TOKEN_<SLUG> resolution)
-- and compiled manifest (role/model/prompt/tool-policy/budget resolution).
-- t.spec_refs (D8) is handed to the runner as AF_SPEC_REFS, verbatim JSON —
-- af-context resolves it against the worktree, never this handler.
SELECT
    t.id AS task_id, t.title, t.intent, t.acceptance_criteria, t.spec_refs,
    t.parent_run_id, t.role,
    p.repos[1]::text AS repo_url,
    p.slug AS project_slug, p.manifest AS project_manifest
FROM task t
JOIN feature f ON f.id = t.feature_id
JOIN project p ON p.id = f.project_id
WHERE t.id = $1;

-- name: SetRunPromptVersion :exec
-- internal/supervisor.Handlers.RunLaunch records which prompts/<role>@vN
-- (internal/domain/prompts) was prepended to this run's TASK — an audit
-- column, not a resolution key.
UPDATE run SET prompt_version = $2 WHERE id = $1;

-- name: IncrementRunEventSeq :one
-- Bumps next_event_seq (under the row lock the caller already holds via
-- GetRunForUpdate) without touching run.state — internal/store.RecordEvent
-- uses this for control-plane events that accompany no state transition
-- (e.g. a mediated tool-dispatch policy decision), so seq allocation stays
-- race-free the same way ApplyRunTransition/ApplyTaskTransition's is, without
-- forcing every event to pretend to be a state change.
UPDATE run SET next_event_seq = next_event_seq + 1, updated_at = now() WHERE id = $1
RETURNING *;

-- name: CostByFeature :many
-- GET /v1/metrics/cost (M7, development-plan.md §11 "cost per merged PR" and
-- the cost dashboard's feature breakdown). Grouped over every run ever
-- created, not just active ones — a dashboard, not a live-run view.
SELECT f.id AS feature_id, f.slug AS feature_slug, p.slug AS project_slug,
       sum(r.cost_usd)::numeric AS cost_usd,
       sum(r.tokens_in)::bigint AS tokens_in,
       sum(r.tokens_out)::bigint AS tokens_out,
       count(*)::bigint AS runs
FROM run r
JOIN task t ON t.id = r.task_id
JOIN feature f ON f.id = t.feature_id
JOIN project p ON p.id = f.project_id
GROUP BY f.id, f.slug, p.slug
ORDER BY cost_usd DESC;

-- name: CostByRole :many
-- GET /v1/metrics/cost's role breakdown — D15's reviewer-vs-implementer
-- model-family split shows up here as two very different cost profiles.
SELECT r.role,
       sum(r.cost_usd)::numeric AS cost_usd,
       sum(r.tokens_in)::bigint AS tokens_in,
       sum(r.tokens_out)::bigint AS tokens_out,
       count(*)::bigint AS runs
FROM run r
GROUP BY r.role
ORDER BY cost_usd DESC;

-- name: CostByModel :many
-- GET /v1/metrics/cost's model breakdown.
SELECT r.model,
       sum(r.cost_usd)::numeric AS cost_usd,
       sum(r.tokens_in)::bigint AS tokens_in,
       sum(r.tokens_out)::bigint AS tokens_out,
       count(*)::bigint AS runs
FROM run r
GROUP BY r.model
ORDER BY cost_usd DESC;

-- name: TotalCostUSD :one
-- GET /v1/metrics/cost's total, and the numerator of §11's cost-per-merged-PR.
SELECT COALESCE(sum(cost_usd), 0)::numeric FROM run;

-- name: CountDoneTasksWithArtifact :one
-- §11 "cost per merged PR — the only cost number that means anything":
-- the denominator. A DONE task with at least one artifact is the closest
-- proxy this schema has to "merged PR" without a control-plane call to
-- api.github.com.
SELECT count(DISTINCT t.id)::bigint
FROM task t
JOIN artifact a ON a.task_id = t.id
WHERE t.state = 'DONE';

-- name: RecordRunUsage :one
-- M4's usage handler (POST /v1/runs/{id}/usage, internal/api/usage.go):
-- tokens/cost accumulate on the run row itself, so the budget check always
-- sees the run's own running total, not just the latest delta a client
-- reported.
UPDATE run
SET tokens_in = tokens_in + $2, tokens_out = tokens_out + $3, cost_usd = cost_usd + $4,
    version = version + 1, updated_at = now()
WHERE id = $1
RETURNING *;
