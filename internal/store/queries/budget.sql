-- name: UpsertBudgetCaps :one
-- Seeds a scope's caps on first use (internal/api's usage handler calls
-- this before every IncrementBudgetSpent) — the manifest compiler that will
-- own real per-project caps doesn't exist until M6, so cmd/control-plane's
-- process-wide default Caps is what flows in here today, same documented
-- stand-in as internal/api.Server.Manifest. ON CONFLICT DO UPDATE with a
-- self-assignment (not DO NOTHING) is the standard sqlc/Postgres trick to
-- still get a RETURNING row on the conflict path.
INSERT INTO budget (scope_kind, scope_id, usd_cap, minute_cap, question_cap)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (scope_kind, scope_id) DO UPDATE SET usd_cap = budget.usd_cap
RETURNING *;

-- name: IncrementBudgetSpent :one
UPDATE budget
SET usd_spent = usd_spent + $3, minutes_spent = minutes_spent + $4, questions_asked = questions_asked + $5,
    updated_at = now()
WHERE scope_kind = $1 AND scope_id = $2
RETURNING *;

-- name: MarkBudgetBreached :exec
UPDATE budget SET breached_at = now() WHERE scope_kind = $1 AND scope_id = $2 AND breached_at IS NULL;
