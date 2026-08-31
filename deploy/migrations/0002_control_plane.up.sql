-- M2: closes the gaps between 0001_init's schema and the durability
-- invariants development-plan.md §3/§7 and docs/adr/{0001,0010} require.
-- Never edit 0001_init.up.sql — this is additive, by design (see
-- .claude/CLAUDE.md: "state transitions ... write an event", "no ORM").

-- ============================================================
-- 1. State/enum CHECK constraints + defaults.
--
-- These duplicate the state lists that live authoritatively in
-- internal/domain (Go). That duplication is deliberate: it makes "illegal
-- transitions error, never silently no-op" true at the storage layer too,
-- not only in application code. Adding a new state needs a migration
-- either way once internal/domain's table changes.
--
-- Sourcing, so a reviewer can tell invention from citation:
--   GROUNDED  — the literal value set is given verbatim in
--               development-plan.md and MUST match exactly.
--   DECISION  — development-plan.md names the column but not its values;
--               this migration picks a set as an implementation choice.
--               internal/domain (P2, next) must use these exact same
--               strings — that's what makes the CHECK meaningful rather
--               than a second, independently-drifting source of truth.
--
-- event.kind gets NO check, deliberately: its vocabulary tracks dsh's own
-- SessionEvent kinds, which change across dsh upgrades (D12, M4.5). Pinning
-- it here would turn a routine dsh bump into a required migration.
-- ============================================================

-- DECISION — project.status is named (§3: "project id, slug, ... status")
-- but never enumerated.
ALTER TABLE project ADD CONSTRAINT project_status_ck CHECK (status IN
    ('ACTIVE', 'PAUSED', 'ARCHIVED'));
ALTER TABLE project ALTER COLUMN status SET DEFAULT 'ACTIVE';

-- DECISION — feature.state is named (§3) but never enumerated.
ALTER TABLE feature ADD CONSTRAINT feature_state_ck CHECK (state IN
    ('OPEN', 'CLOSED'));
ALTER TABLE feature ALTER COLUMN state SET DEFAULT 'OPEN';

-- GROUNDED — §3's task state machine diagram, verbatim:
--   CREATED -> QUEUED -> RUNNING -> REVIEW -> DONE
--   RUNNING -> BLOCKED_ON_HUMAN -> RUNNING (loop back)
--   RUNNING -> FAILED / CANCELLED / PARKED
ALTER TABLE task ADD CONSTRAINT task_state_ck CHECK (state IN
    ('CREATED', 'QUEUED', 'RUNNING', 'REVIEW', 'DONE',
     'BLOCKED_ON_HUMAN', 'FAILED', 'CANCELLED', 'PARKED'));
ALTER TABLE task ALTER COLUMN state SET DEFAULT 'CREATED';
-- DECISION (partial) — only 'spec' is textually attested ("if 90% bypasses
-- the spec lane, SDD adoption is cosmetic", §11 Metrics). 'direct' is this
-- migration's name for the complement (a task not routed through Spec
-- Kit); no other lane name appears anywhere in the plan. Revisit once
-- Architect B's actual Spec Kit preset work (§10) pins the real set.
ALTER TABLE task ADD CONSTRAINT task_lane_ck CHECK (lane IN
    ('spec', 'direct'));

-- DECISION — run.state is named (§3) but never enumerated.
ALTER TABLE run ADD CONSTRAINT run_state_ck CHECK (state IN
    ('PENDING', 'STARTING', 'RUNNING', 'EXITED', 'FAILED', 'CANCELLED'));
ALTER TABLE run ALTER COLUMN state SET DEFAULT 'PENDING';
-- GROUNDED — §5's Model allocation table (Orchestrator / Implementer /
-- Reviewer / Classifier row labels) and the manifest example's literal
-- `implementer:` key (§5 "Manifest compiles to a patch").
ALTER TABLE run ADD CONSTRAINT run_role_ck CHECK (role IN
    ('orchestrator', 'implementer', 'reviewer', 'classifier'));

-- DECISION — question.state is named (§3) but never enumerated. (The
-- timeout ladder in §6 — nudge 4h / escalate 24h / park 72h — acts on the
-- *task*, not necessarily a distinct question.state value; not reused
-- here to avoid asserting a mapping the plan doesn't make.)
ALTER TABLE question ADD CONSTRAINT question_state_ck CHECK (state IN
    ('OPEN', 'ANSWERED', 'TIMED_OUT', 'CANCELLED'));
ALTER TABLE question ALTER COLUMN state SET DEFAULT 'OPEN';
-- GROUNDED — §6 Zulip mechanics, verbatim: "Emoji reaction for
-- choice/confirm; topic reply for free_text."
ALTER TABLE question ADD CONSTRAINT question_kind_ck CHECK (kind IN
    ('choice', 'confirm', 'free_text'));

-- DECISION — approval.decision is named (§4: "POST /v1/approvals
-- {subject_ref, sha256, decision}") but never enumerated.
ALTER TABLE approval ADD CONSTRAINT approval_decision_ck CHECK (decision IN
    ('APPROVED', 'REJECTED'));
-- DECISION (partial) — 'spec' and 'plan' are grounded (§7 M8: "approval
-- gates on spec.md and plan.md"); 'pr' is this migration's addition for
-- M4's pre-merge human approval (D3's four-layer merge-prevention story),
-- not itself a literal token in the plan text.
ALTER TABLE approval ADD CONSTRAINT approval_subject_kind_ck CHECK (subject_kind IN
    ('spec', 'plan', 'pr'));

-- DECISION — event.actor is unenumerated; 'control_plane' is required (it
-- is event.source='control_plane' rows' actor for control-plane-native
-- transitions), the rest are a reasonable minimal set for who/what caused
-- an event.
ALTER TABLE event ADD CONSTRAINT event_actor_ck CHECK (actor IN
    ('agent', 'tool', 'user', 'system', 'control_plane'));

-- DECISION — artifact.kind is named (§3) but never enumerated.
ALTER TABLE artifact ADD CONSTRAINT artifact_kind_ck CHECK (kind IN
    ('pr', 'diff', 'log', 'spec'));

-- DECISION (partial) — 'run' and 'feature' are grounded (§6 Budgets as
-- signal: "Cap questions per run (3) and per feature (10)"). 'project' is
-- not textually attested as a budget scope anywhere; included anyway as
-- the natural third rollup level — drop it here if that turns out wrong
-- rather than silently allowing an unscoped budget row.
ALTER TABLE budget ADD CONSTRAINT budget_scope_kind_ck CHECK (scope_kind IN
    ('run', 'feature', 'project'));

-- DECISION — identity.kind/role are named (§3) but never enumerated as a
-- closed set. 'human'/'agent' covers §6's "Verify the sender maps to a
-- known identity" (human Zulip senders) plus bot/agent identities implied
-- by "Zulip bots created" (§7 M0). Role values echo §10's Team table
-- (Architect, Developer) plus §5's agent roles, since §6 addresses humans
-- "by role — requirements to the architect, implementation to the
-- assigned developer."
ALTER TABLE identity ADD CONSTRAINT identity_kind_ck CHECK (kind IN
    ('human', 'agent'));
ALTER TABLE identity ADD CONSTRAINT identity_role_ck CHECK (role IN
    ('architect', 'developer', 'orchestrator', 'implementer', 'reviewer'));

-- ============================================================
-- 2. event(run_id, seq) uniqueness — the load-bearing change.
--
-- docs/adr/0001: "af-control must make the mirror idempotent — replays
-- after a crash or reconnect must not double-write event rows. This is a
-- real invariant to test, not a formality." 0001_init only ever had a
-- non-unique index here; nothing enforced it.
--
-- Two disjoint id spaces share the `seq` column: dsh's own monotonic
-- per-session SessionEvent.seq (source='dsh', stable across replays — the
-- ON CONFLICT DO NOTHING below is what makes af-control's retry safe) and
-- control-plane-native seqs allocated from run.next_event_seq /
-- task.next_event_seq inside the transition transaction (source=
-- 'control_plane', deduped separately via an explicit key since they are
-- emitted from an at-least-once outbox-adjacent path, not a replay-stable
-- log).
-- ============================================================

ALTER TABLE event ADD COLUMN source text NOT NULL DEFAULT 'control_plane'
    CHECK (source IN ('dsh', 'control_plane'));
ALTER TABLE event ADD COLUMN dedupe_key text;

DROP INDEX event_run_id_seq_idx;

CREATE UNIQUE INDEX event_dsh_seq_uk ON event (run_id, seq)
    WHERE source = 'dsh' AND run_id IS NOT NULL;

CREATE UNIQUE INDEX event_dedupe_uk ON event (dedupe_key)
    WHERE dedupe_key IS NOT NULL;

ALTER TABLE event ADD CONSTRAINT event_scope_ck CHECK (run_id IS NOT NULL OR task_id IS NOT NULL);

CREATE INDEX event_task_at_idx ON event (task_id, at);
-- Total read order for GET /v1/events?since= (SSE cursor); (at, id) rather
-- than id alone because a client resumes from a timestamp, not an opaque id.
CREATE INDEX event_at_id_idx ON event (at, id);

-- ============================================================
-- 3. Append-only enforcement.
--
-- .claude/CLAUDE.md: "event table is append-only — never update or delete
-- rows in it." Convention-only until now.
-- ============================================================

CREATE FUNCTION event_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'event is append-only (development-plan.md §3, .claude/CLAUDE.md)';
END;
$$;

CREATE TRIGGER event_no_mutate
    BEFORE UPDATE OR DELETE ON event
    FOR EACH STATEMENT EXECUTE FUNCTION event_append_only();

-- ============================================================
-- 4. Outbox retry/backoff/poison affordance.
--
-- docs/adr/0010: the transactional outbox is what earns "killing the
-- control plane mid-run loses nothing" — 0001_init's outbox had no way to
-- retry, back off, or give up (poison) on a failing effect.
-- ============================================================

ALTER TABLE outbox
    ADD COLUMN key          text,
    ADD COLUMN attempts     integer     NOT NULL DEFAULT 0,
    ADD COLUMN last_error   text,
    ADD COLUMN available_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN failed_at    timestamptz,
    ADD COLUMN created_at   timestamptz NOT NULL DEFAULT now();

-- key is what makes ENQUEUE idempotent (e.g. "launch:<run_id>"); a
-- transition retried after a crash re-derives the same key and the second
-- INSERT ... ON CONFLICT (key) DO NOTHING is a no-op. Delivery idempotency
-- (the relay side) is a separate concern the consumer's Key contract owns.
--
-- This is a PARTIAL unique index (WHERE key IS NOT NULL). Postgres requires
-- an ON CONFLICT target to name the exact same predicate, or it errors with
-- "no unique or exclusion constraint matching the ON CONFLICT specification"
-- instead of silently falling back to a full-table check — verified live
-- against this migration. internal/store's outbox-enqueue query (P3) must
-- read: INSERT INTO outbox (...) VALUES (...) ON CONFLICT (key) WHERE key
-- IS NOT NULL DO NOTHING.
CREATE UNIQUE INDEX outbox_key_uk ON outbox (key) WHERE key IS NOT NULL;

DROP INDEX outbox_unpublished_idx;
CREATE INDEX outbox_claimable_idx ON outbox (available_at, id)
    WHERE published_at IS NULL AND failed_at IS NULL;

-- ============================================================
-- 5. Optimistic concurrency + run/task bookkeeping.
-- ============================================================

ALTER TABLE task
    ADD COLUMN version        integer     NOT NULL DEFAULT 0,
    ADD COLUMN updated_at     timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN next_event_seq bigint      NOT NULL DEFAULT 0;

ALTER TABLE run
    ADD COLUMN version           integer     NOT NULL DEFAULT 0,
    ADD COLUMN updated_at        timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN next_event_seq    bigint      NOT NULL DEFAULT 0,
    -- sha256 of the per-run bearer token; the plaintext is handed to the
    -- supervisor for container env injection and never persisted anywhere
    -- (development-plan.md §8 Secrets: "never ... in the manifest, in agent
    -- context, or in events").
    ADD COLUMN token_hash        bytea,
    ADD COLUMN last_heartbeat_at timestamptz,
    ADD COLUMN attempt           integer     NOT NULL DEFAULT 0,
    ADD COLUMN exit_code         integer;

-- ============================================================
-- 6. Ingestion idempotency: external_ref is the tasks.md-side identity a
-- re-POST resolves against.
-- ============================================================

CREATE UNIQUE INDEX task_feature_external_ref_uk ON task (feature_id, external_ref)
    WHERE external_ref IS NOT NULL;

-- ============================================================
-- 7. One live (non-terminal) run per task.
--
-- Stricter than D2 strictly requires, in exchange for making a redelivered
-- run.launch outbox message structurally unable to start a second run for
-- the same task — free idempotency at the schema layer, on top of Podman's
-- own create-by-name idempotency (internal/podman.Client.Create).
--
-- M5's af-subagent fan-out (multiple concurrent child runs under one
-- orchestrator run) will need this relaxed to
-- `WHERE parent_run_id IS NULL AND state IN (...)` — not needed before M5,
-- not solved here.
-- ============================================================

CREATE UNIQUE INDEX run_active_per_task_uk ON run (task_id)
    WHERE state IN ('PENDING', 'STARTING', 'RUNNING');

-- ============================================================
-- 8. Identity uniqueness.
-- ============================================================

CREATE UNIQUE INDEX identity_zulip_user_id_uk ON identity (zulip_user_id)
    WHERE zulip_user_id IS NOT NULL;
CREATE UNIQUE INDEX identity_github_login_uk ON identity (github_login)
    WHERE github_login IS NOT NULL;

-- ============================================================
-- 9. Re-ingesting an identical tasks.md is a no-op, not a re-validation.
-- ============================================================

ALTER TABLE feature ADD COLUMN tasks_md_sha256 text;
