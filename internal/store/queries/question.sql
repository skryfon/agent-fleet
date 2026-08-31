-- name: CreateQuestion :one
INSERT INTO question (run_id, task_id, kind, body, options, addressee, state)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetQuestionByID :one
SELECT * FROM question WHERE id = $1;

-- name: ListOpenQuestionsByRun :many
-- The inbox long-poll (GET /v1/runs/{id}/inbox, P4) reads this to build the
-- answer envelope once M3's af-ask-human lands; the envelope shape exists
-- now, only the producer side (M3) is deferred.
SELECT * FROM question WHERE run_id = $1 AND state = 'OPEN' ORDER BY asked_at;

-- name: AnswerQuestion :one
UPDATE question SET state = 'ANSWERED', answer = $2, answered_by = $3, answered_at = now()
WHERE id = $1
RETURNING *;
