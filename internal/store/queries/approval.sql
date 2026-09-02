-- name: InsertApproval :one
-- POST /v1/approvals (development-plan.md §4/§3). No update path — every
-- decision is its own row, per the same append-mostly instinct the `event`
-- table follows (a re-decision is a new approval, not a mutated one).
INSERT INTO approval (subject_kind, subject_ref, subject_sha256, decision, actor, note)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;
