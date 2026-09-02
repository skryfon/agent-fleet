-- M5: orchestration (development-plan.md §5/§7 M5). Additive over 0001-0004,
-- per .claude/CLAUDE.md's "no ORM" / never-edit-a-shipped-migration
-- convention.
--
-- Fan-out shape (decided in the M5 plan): spawn_worker creates a CHILD TASK
-- row in the same feature, not a sibling run under the parent's own task.
-- This is why 0002_control_plane.up.sql's own note ("M5's af-subagent
-- fan-out ... will need [run_active_per_task_uk] relaxed to `WHERE
-- parent_run_id IS NULL AND state IN (...)`") turns out NOT to be needed:
-- with one task per worker, "one active run per task" still holds exactly
-- as written. That note's premise (sibling runs under one task) was an
-- earlier guess this migration supersedes; left uncorrected in 0002 itself
-- per the never-edit-a-shipped-migration rule, corrected here instead.

-- ============================================================
-- 1. Child task bookkeeping.
-- ============================================================

-- parent_run_id names the orchestrator (or, transitively, a worker) run
-- whose spawn_worker call created this task — NOT a foreign key to the
-- parent's own task row, because a "parent task" isn't a stable concept
-- here (a task's own several run attempts could each spawn children); the
-- run that actually did the spawning is what CancelSubtree's recursive walk
-- (internal/store, Phase 2) needs.
ALTER TABLE task ADD COLUMN parent_run_id uuid REFERENCES run (id);

-- depth is the same "how many spawn_worker hops from the feature's own
-- root task" internal/fanout.Check enforces against MaxDepth — stored
-- (not recomputed by walking parent_run_id every time) since it is read on
-- every spawn_worker dispatch, a hot path relative to how rarely it
-- changes.
ALTER TABLE task ADD COLUMN depth integer NOT NULL DEFAULT 0;

-- role: NULL means "use internal/supervisor.Handlers.DefaultRole" — a
-- spawn_worker call may request a role (development-plan.md §5's manifest
-- example lists reviewer/implementer/classifier), but most callers don't
-- care and get the process-wide default, same stand-in as
-- api.Server.Manifest/BudgetCaps until M6's manifest compiler.
ALTER TABLE task ADD COLUMN role text;

CREATE INDEX task_parent_run_idx ON task (parent_run_id) WHERE parent_run_id IS NOT NULL;

-- ============================================================
-- 2. Worker -> orchestrator questions (D7, ask_orchestrator).
--
-- These must never reach Zulip (D7: "only the orchestrator gets ask_human")
-- and must not contend with a human-facing question for
-- "one open question per feature at a time" (§6) — N parallel workers each
-- asking their orchestrator would otherwise deadlock on that constraint,
-- since they all share the same feature_id.
-- ============================================================

ALTER TABLE question ADD COLUMN to_run_id uuid REFERENCES run (id);

DROP INDEX question_one_open_per_feature_uk;
CREATE UNIQUE INDEX question_one_open_per_feature_uk ON question (feature_id)
    WHERE state = 'OPEN' AND to_run_id IS NULL;

-- A worker may have at most one open question to ITS orchestrator at a
-- time — same "queue the rest" discipline as the human-facing case, scoped
-- per asking run instead of per feature.
CREATE UNIQUE INDEX question_one_open_per_run_uk ON question (run_id)
    WHERE state = 'OPEN' AND to_run_id IS NOT NULL;

CREATE INDEX question_to_run_idx ON question (to_run_id) WHERE to_run_id IS NOT NULL;

-- ============================================================
-- 3. run_inbox: durable queue for anything the control plane hands a
-- running container that isn't derivable from run.state alone (a worker's
-- question, a worker's report). GET /v1/runs/{id}/inbox's existing "cancel"
-- delivery stays derived from run.state — this table is additive, not a
-- replacement.
-- ============================================================

CREATE TABLE run_inbox (
    id           bigserial PRIMARY KEY,
    run_id       uuid NOT NULL REFERENCES run (id),
    kind         text NOT NULL CHECK (kind IN ('worker_question', 'worker_report')),
    payload      jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz
);

-- What the inbox long-poll claims from, oldest first, skipping rows another
-- concurrent poller already claimed (FOR UPDATE SKIP LOCKED at the query
-- layer — internal/store/queries/run_inbox.sql).
CREATE INDEX run_inbox_pending_idx ON run_inbox (run_id, id) WHERE delivered_at IS NULL;
