DROP TABLE run_inbox;

DROP INDEX question_to_run_idx;
DROP INDEX question_one_open_per_run_uk;
DROP INDEX question_one_open_per_feature_uk;
CREATE UNIQUE INDEX question_one_open_per_feature_uk ON question (feature_id)
    WHERE state = 'OPEN';
ALTER TABLE question DROP COLUMN to_run_id;

DROP INDEX task_parent_run_idx;
ALTER TABLE task DROP COLUMN role;
ALTER TABLE task DROP COLUMN depth;
ALTER TABLE task DROP COLUMN parent_run_id;
