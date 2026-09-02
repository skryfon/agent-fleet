-- name: EnqueueRunInbox :one
-- Durable queue entry for a worker_question/worker_report handed to a
-- specific run (M5, development-plan.md §5 ask_orchestrator/
-- report_to_orchestrator) — GET /v1/runs/{id}/inbox's long-poll claims
-- these via ClaimNextRunInbox below, in addition to its existing run.state
-- derived "cancel" delivery.
INSERT INTO run_inbox (run_id, kind, payload)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ClaimNextRunInbox :one
-- Claims (marks delivered_at) and returns the oldest still-pending row for
-- run_id, or no rows if the inbox is empty. FOR UPDATE SKIP LOCKED so two
-- concurrent long-poll requests for the same run (a redelivered HTTP retry)
-- never claim, and therefore never deliver, the same row twice.
UPDATE run_inbox
SET delivered_at = now()
WHERE id = (
    SELECT id FROM run_inbox
    WHERE run_id = $1 AND delivered_at IS NULL
    ORDER BY id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;
