-- M3: the question lifecycle's remaining schema gaps
-- (development-plan.md §6, §7 M3). Additive over 0001/0002, per
-- .claude/CLAUDE.md's "no ORM" / append-only conventions — never edit an
-- already-shipped migration.

-- feature_id is denormalized onto question (rather than joined through
-- task) so "one open question per topic at a time" (§6) can be a single
-- partial unique index below, not an app-level check racing a concurrent
-- ask_human call.
ALTER TABLE question ADD COLUMN feature_id uuid REFERENCES feature(id);

-- zulip_message_id: what internal/zulip.Handlers.Notify persists after
-- posting, making a redelivered outbox row a no-op (outbox.Handler's
-- idempotency contract — Zulip itself has no dedupe of its own).
ALTER TABLE question ADD COLUMN zulip_message_id text;

-- nudged_at/escalated_at: the timeout ladder's own bookkeeping (nudge 4h /
-- escalate 24h / park 72h, §6) — asked_at alone can't tell a sweeper
-- whether the 4h nudge already fired.
ALTER TABLE question ADD COLUMN nudged_at timestamptz;
ALTER TABLE question ADD COLUMN escalated_at timestamptz;

-- GROUNDED — §6 "One open question per topic at a time. Queue the rest."
-- Enforced at the database, not application code: a second ask_human call
-- against the same feature while one question is still OPEN fails the
-- INSERT outright.
CREATE UNIQUE INDEX question_one_open_per_feature_uk ON question (feature_id)
    WHERE state = 'OPEN';

CREATE INDEX question_feature_idx ON question (feature_id);
