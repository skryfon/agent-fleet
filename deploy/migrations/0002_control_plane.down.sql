-- Reverses 0002_control_plane.up.sql, in reverse group order.

-- 9.
ALTER TABLE feature DROP COLUMN tasks_md_sha256;

-- 8.
DROP INDEX identity_github_login_uk;
DROP INDEX identity_zulip_user_id_uk;

-- 7.
DROP INDEX run_active_per_task_uk;

-- 6.
DROP INDEX task_feature_external_ref_uk;

-- 5.
DROP INDEX question_run_state_idx;
DROP INDEX task_state_created_idx;

ALTER TABLE run
    DROP COLUMN exit_code,
    DROP COLUMN attempt,
    DROP COLUMN last_heartbeat_at,
    DROP COLUMN token_hash,
    DROP COLUMN next_event_seq,
    DROP COLUMN updated_at,
    DROP COLUMN version;

ALTER TABLE task
    DROP COLUMN attempt,
    DROP COLUMN next_event_seq,
    DROP COLUMN updated_at,
    DROP COLUMN version;

-- 4.
DROP INDEX outbox_claimable_idx;
CREATE INDEX outbox_unpublished_idx ON outbox (id) WHERE published_at IS NULL;

-- outbox_key_uk would also disappear implicitly once `key` is dropped below
-- (Postgres auto-drops an index when its column goes) — explicit here so
-- the down script doesn't rely on a reader knowing that.
DROP INDEX outbox_key_uk;

ALTER TABLE outbox
    DROP COLUMN created_at,
    DROP COLUMN failed_at,
    DROP COLUMN available_at,
    DROP COLUMN last_error,
    DROP COLUMN attempts,
    DROP COLUMN key;

-- 3.
DROP TRIGGER event_no_mutate ON event;
DROP FUNCTION event_append_only();

-- 2.
DROP INDEX event_at_id_idx;
DROP INDEX event_task_at_idx;
ALTER TABLE event DROP CONSTRAINT event_scope_ck;
DROP INDEX event_dedupe_uk;
DROP INDEX event_dsh_seq_uk;
CREATE INDEX event_run_id_seq_idx ON event (run_id, seq);
ALTER TABLE event
    DROP COLUMN dedupe_key,
    DROP COLUMN source;

-- 1.
ALTER TABLE identity DROP CONSTRAINT identity_role_ck;
ALTER TABLE identity DROP CONSTRAINT identity_kind_ck;
ALTER TABLE budget DROP CONSTRAINT budget_scope_kind_ck;
ALTER TABLE artifact DROP CONSTRAINT artifact_kind_ck;
ALTER TABLE event DROP CONSTRAINT event_actor_ck;
ALTER TABLE approval DROP CONSTRAINT approval_subject_kind_ck;
ALTER TABLE approval DROP CONSTRAINT approval_decision_ck;
ALTER TABLE question DROP CONSTRAINT question_kind_ck;
ALTER TABLE question ALTER COLUMN state DROP DEFAULT;
ALTER TABLE question DROP CONSTRAINT question_state_ck;
ALTER TABLE run DROP CONSTRAINT run_role_ck;
ALTER TABLE run ALTER COLUMN state DROP DEFAULT;
ALTER TABLE run DROP CONSTRAINT run_state_ck;
ALTER TABLE task DROP CONSTRAINT task_lane_ck;
ALTER TABLE task ALTER COLUMN state DROP DEFAULT;
ALTER TABLE task DROP CONSTRAINT task_state_ck;
ALTER TABLE feature ALTER COLUMN state DROP DEFAULT;
ALTER TABLE feature DROP CONSTRAINT feature_state_ck;
ALTER TABLE project ALTER COLUMN status DROP DEFAULT;
ALTER TABLE project DROP CONSTRAINT project_status_ck;
