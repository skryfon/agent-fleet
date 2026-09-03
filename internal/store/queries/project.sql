-- name: CreateProject :one
-- COALESCE(..., '{}'), not a bare bind: a caller that leaves
-- CreateProjectParams.Manifest at its zero value (nil []byte, which pgx
-- sends as SQL NULL for jsonb) gets the same '{}' no-manifest default
-- 0006_m6.up.sql's column DEFAULT would give an omitted column — every
-- fixture/test written before M6 keeps working unchanged. internal/api's
-- createProject always passes an already-parsed, already-hashed manifest
-- once M6's manifest.Parse validates the request body, which simply wins
-- over the fallback.
INSERT INTO project (slug, manifest_ref, manifest_hash, repos, status, manifest)
VALUES ($1, $2, $3, $4, $5, COALESCE(sqlc.arg(manifest)::jsonb, '{}'::jsonb))
RETURNING *;

-- name: GetProjectByID :one
SELECT * FROM project WHERE id = $1;

-- name: GetProjectBySlug :one
SELECT * FROM project WHERE slug = $1;

-- name: ListProjects :many
SELECT * FROM project ORDER BY created_at DESC;

-- name: GetManifestForRun :one
-- internal/api's resolveManifest (M6): project_id lives only on feature
-- today (0001_init.up.sql) — this walk is the same run -> task -> feature
-- -> project join internal/store/queries/run.sql's GetLaunchContext
-- already does; project_id is deliberately NOT denormalized onto run/task,
-- nothing else needs it and this join is indexed on every FK it crosses.
SELECT p.manifest
FROM run r
JOIN task t ON t.id = r.task_id
JOIN feature f ON f.id = t.feature_id
JOIN project p ON p.id = f.project_id
WHERE r.id = $1;

-- name: UpdateProjectManifest :one
-- PUT /v1/projects/{slug}/manifest (M6): a manifest revision is not a
-- DELETE + re-register. manifest_ref/manifest_hash move together with the
-- new manifest — never let manifest_hash drift from what's actually stored.
UPDATE project
SET manifest = $2, manifest_ref = $3, manifest_hash = $4
WHERE slug = $1
RETURNING *;
