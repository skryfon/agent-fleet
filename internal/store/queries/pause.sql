-- name: UpsertPause :one
-- POST /v1/admin/pause. ON CONFLICT re-stamps actor/reason/paused_at rather
-- than erroring — re-pausing an already-paused scope (e.g. a second admin
-- hitting the kill switch) is idempotent, not a 409.
INSERT INTO pause (scope, actor, reason)
VALUES ($1, $2, $3)
ON CONFLICT (scope) DO UPDATE SET actor = excluded.actor, reason = excluded.reason, paused_at = now()
RETURNING *;

-- name: DeletePause :exec
-- DELETE /v1/admin/pause?scope=... (resume).
DELETE FROM pause WHERE scope = $1;

-- name: GetPause :one
SELECT * FROM pause WHERE scope = $1;
