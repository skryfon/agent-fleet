-- name: InsertApproval :one
-- POST /v1/approvals (development-plan.md §4/§3). No update path — every
-- decision is its own row, per the same append-mostly instinct the `event`
-- table follows (a re-decision is a new approval, not a mutated one).
INSERT INTO approval (subject_kind, subject_ref, subject_sha256, decision, actor, note)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListPendingApprovals :many
-- GET /v1/approvals/pending (M7): every task sitting in REVIEW. Deliberately
-- no artifact join here — sqlc/pgx typed a LEFT JOIN LATERAL's output
-- columns as non-nullable despite real NULLs (a REVIEW task with no artifact
-- yet is a real state), which crashes the scan; the handler instead calls
-- the existing GetLatestArtifactByTask (artifact.sql) per task, the same
-- query reviewByZulipTopic already uses, and treats ErrNoRows as "no
-- artifact" rather than an error.
SELECT t.id AS task_id, t.title, t.intent, t.acceptance_criteria, t.lane,
       t.role, t.updated_at,
       f.id AS feature_id, f.slug AS feature_slug, f.zulip_topic,
       p.slug AS project_slug
FROM task t
JOIN feature f ON f.id = t.feature_id
JOIN project p ON p.id = f.project_id
WHERE t.state = 'REVIEW'
ORDER BY t.updated_at;
