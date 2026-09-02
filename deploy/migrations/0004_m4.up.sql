-- M4: policy, approvals, safety (development-plan.md §4/§7 M4). Additive
-- over 0001-0003, per .claude/CLAUDE.md's "no ORM" / never-edit-a-shipped-
-- migration convention.

-- budget rows (table exists since 0001_init) need a last-write marker for
-- internal/api's usage handler to reason about staleness, and a breach
-- marker so a scope that already tripped isn't re-notified every POST.
ALTER TABLE budget ADD COLUMN updated_at  timestamptz NOT NULL DEFAULT now();
ALTER TABLE budget ADD COLUMN breached_at timestamptz;

-- The kill switch (§4: "POST /v1/admin/pause  global + per-project kill
-- switch"). scope is a plain text key ("global" or "project:<uuid>") rather
-- than a (kind, id) pair — one table, one lookup, no nullable-uuid PK;
-- ponytail: split it if a third scope kind ever appears.
CREATE TABLE pause (
    scope     text PRIMARY KEY,
    actor     text NOT NULL,
    reason    text,
    paused_at timestamptz NOT NULL DEFAULT now()
);
