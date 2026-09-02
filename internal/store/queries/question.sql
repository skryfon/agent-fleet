-- name: CreateQuestion :one
-- feature_id (0003_questions.up.sql) is what question_one_open_per_feature_uk
-- enforces against — a second ask_human call for a feature with an already-
-- OPEN question fails this INSERT with a unique_violation, which
-- internal/store.ApplyAsk turns into ErrQuestionAlreadyOpen.
--
-- to_run_id (0005_m5.up.sql) is NULL for a human-facing ask_human — it is
-- only set for D7's ask_orchestrator, naming the orchestrator run the
-- question is routed to instead of Zulip; question_one_open_per_feature_uk
-- is scoped WHERE to_run_id IS NULL so worker->orchestrator questions never
-- contend with each other or with the feature's own human-facing slot
-- (question_one_open_per_run_uk is what caps THOSE, one per asking run).
INSERT INTO question (run_id, task_id, feature_id, kind, body, options, addressee, state, to_run_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetQuestionByID :one
SELECT * FROM question WHERE id = $1;

-- name: GetQuestionForUpdate :one
-- Locks the row for internal/api.answerQuestion's read-check-write (must
-- still be OPEN) the same way GetTaskForUpdate/GetRunForUpdate lock theirs.
SELECT * FROM question WHERE id = $1 FOR UPDATE;

-- name: ListOpenQuestionsByRun :many
-- The inbox long-poll (GET /v1/runs/{id}/inbox) reads this to build the
-- answer envelope.
SELECT * FROM question WHERE run_id = $1 AND state = 'OPEN' ORDER BY asked_at;

-- name: ListOpenQuestionsByFeature :many
-- The Zulip bridge's inbound path resolves a topic reply to the one open
-- question a feature can have (question_one_open_per_feature_uk).
SELECT * FROM question WHERE feature_id = $1 AND state = 'OPEN' ORDER BY asked_at;

-- name: ListOverdueQuestions :many
-- internal/questions' timeout-ladder sweeper: every still-OPEN question
-- past its next unfired rung. The sweeper itself decides nudge/escalate/park
-- from asked_at/nudged_at/escalated_at; this just narrows the scan to rows
-- that could possibly be due, using the earliest rung (4h) as the floor.
SELECT * FROM question WHERE state = 'OPEN' AND asked_at < $1 ORDER BY asked_at;

-- name: MarkQuestionNudged :one
UPDATE question SET nudged_at = now() WHERE id = $1
RETURNING *;

-- name: MarkQuestionEscalated :one
UPDATE question SET escalated_at = now() WHERE id = $1
RETURNING *;

-- name: TimeoutQuestion :one
-- Timeouts never auto-answer (development-plan.md §6) — this only marks the
-- question TIMED_OUT; the accompanying task transition (TrPark) is a
-- separate call in the same transaction, mirroring AnswerQuestion/
-- TrAnswered's split.
UPDATE question SET state = 'TIMED_OUT' WHERE id = $1 AND state = 'OPEN'
RETURNING *;

-- name: CancelQuestionsForRun :many
-- A run that's cancelled or killed shouldn't leave a dangling OPEN question
-- blocking its feature's one-open-per-topic slot forever.
UPDATE question SET state = 'CANCELLED' WHERE run_id = $1 AND state = 'OPEN'
RETURNING *;

-- name: SetQuestionZulipMessageID :one
-- internal/zulip.Handlers.Notify calls this after a successful post, making
-- a redelivered outbox row's re-post a no-op (outbox.Handler's idempotency
-- contract) rather than a duplicate Zulip message.
UPDATE question SET zulip_message_id = $2 WHERE id = $1
RETURNING *;

-- name: AnswerQuestion :one
UPDATE question SET state = 'ANSWERED', answer = $2, answered_by = $3, answered_at = now()
WHERE id = $1 AND state = 'OPEN'
RETURNING *;
