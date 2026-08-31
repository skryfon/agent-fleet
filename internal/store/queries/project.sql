-- name: CreateProject :one
INSERT INTO project (slug, manifest_ref, manifest_hash, repos, status)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetProjectByID :one
SELECT * FROM project WHERE id = $1;

-- name: GetProjectBySlug :one
SELECT * FROM project WHERE slug = $1;

-- name: ListProjects :many
SELECT * FROM project ORDER BY created_at DESC;
