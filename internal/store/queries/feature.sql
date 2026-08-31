-- name: CreateFeature :one
INSERT INTO feature (project_id, slug, spec_ref, zulip_topic, state)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetFeatureByID :one
SELECT * FROM feature WHERE id = $1;

-- name: GetFeatureByProjectSlug :one
SELECT * FROM feature WHERE project_id = $1 AND slug = $2;

-- name: ListFeaturesByProject :many
SELECT * FROM feature WHERE project_id = $1 ORDER BY created_at DESC;

-- name: SetFeatureTasksMdSHA256 :exec
UPDATE feature SET tasks_md_sha256 = $2 WHERE id = $1;
