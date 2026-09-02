-- name: InsertArtifact :one
-- M4: internal/api's pr_opened mediated-tool handler inserts one of these
-- per PR the implementer opens — its sha256 (a diff hash, not the PR body)
-- is what POST /v1/approvals binds an approval to (development-plan.md §3:
-- "approval.subject_sha256 is mandatory ... a revised artifact voids its
-- approval").
INSERT INTO artifact (task_id, kind, uri, sha256)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetArtifactByURI :one
-- POST /v1/approvals looks a PR up by its url (subject_ref) to fetch the
-- sha256 an approval must match.
SELECT * FROM artifact WHERE uri = $1 ORDER BY created_at DESC LIMIT 1;

-- name: GetLatestArtifactByTask :one
-- cmd/bridge's review-by-zulip-topic lookup (M4): a task in REVIEW has
-- exactly one PR artifact in the ordinary case, but this takes the latest
-- if af-github's gh_pr_create ever runs twice for the same task.
SELECT * FROM artifact WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1;
