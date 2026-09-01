-- Reverses 0003_questions.up.sql, in reverse order.

DROP INDEX question_feature_idx;
DROP INDEX question_one_open_per_feature_uk;

ALTER TABLE question DROP COLUMN escalated_at;
ALTER TABLE question DROP COLUMN nudged_at;
ALTER TABLE question DROP COLUMN zulip_message_id;
ALTER TABLE question DROP COLUMN feature_id;
