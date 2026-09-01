-- name: CreateFeature :one
INSERT INTO feature (project_id, slug, spec_ref, zulip_topic, state)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetFeatureByID :one
SELECT * FROM feature WHERE id = $1;

-- name: GetFeatureByProjectSlug :one
SELECT * FROM feature WHERE project_id = $1 AND slug = $2;

-- name: GetFeatureByZulipTopic :one
-- cmd/bridge's inbound path (M3): resolve a Zulip topic reply back to the
-- feature it belongs to. Falls back to matching the feature's own slug
-- (deploy/zulip/README.md §6: "the feature slug ... is a reasonable
-- default" when zulip_topic was never explicitly set at feature creation).
SELECT * FROM feature WHERE zulip_topic = $1 OR (zulip_topic IS NULL AND slug = $1);

-- name: ListFeaturesByProject :many
SELECT * FROM feature WHERE project_id = $1 ORDER BY created_at DESC;

-- name: SetFeatureTasksMdSHA256 :exec
UPDATE feature SET tasks_md_sha256 = $2 WHERE id = $1;
