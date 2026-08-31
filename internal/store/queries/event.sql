-- name: InsertControlPlaneEvent :one
-- The only place source='control_plane' events are written — always inside
-- the same transaction as the state UPDATE and outbox INSERTs it accompanies
-- (internal/store.ApplyTaskTransition, P3). seq comes from the caller, which
-- read it from task.next_event_seq/run.next_event_seq under the same
-- FOR UPDATE lock as the state change, so it is race-free without a
-- separate sequence object.
INSERT INTO event (run_id, task_id, seq, kind, actor, payload, source, dedupe_key)
VALUES ($1, $2, $3, $4, $5, $6, 'control_plane', $7)
ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING
RETURNING *;

-- name: AppendMirrorEvents :execrows
-- The idempotent dsh-session mirror (docs/adr/0001). ON CONFLICT DO NOTHING
-- against event_dsh_seq_uk (run_id, seq) WHERE source='dsh' is what makes a
-- replayed af-control batch, after a crash or reconnect, a no-op. seq is
-- NEVER renumbered by the caller — it is dsh's own SessionEvent.seq, stable
-- across replays, and that stability is exactly what this ON CONFLICT
-- relies on. sqlc emits :execrows so the caller can report accepted vs.
-- duplicate counts back to af-control (POST /v1/runs/{id}/events' response
-- body).
INSERT INTO event (run_id, task_id, seq, kind, actor, payload, at, source)
SELECT sqlc.arg(run_id)::uuid, sqlc.arg(task_id)::uuid, s.seq, s.kind, s.actor, s.payload, s.at, 'dsh'
FROM unnest(
    sqlc.arg(seqs)::bigint[],
    sqlc.arg(kinds)::text[],
    sqlc.arg(actors)::text[],
    sqlc.arg(payloads)::jsonb[],
    sqlc.arg(ats)::timestamptz[]
) AS s(seq, kind, actor, payload, at)
ON CONFLICT DO NOTHING;

-- name: MirrorHighWaterSeq :one
-- The value af-control seeds highWaterSeq from on (re)connect — never
-- guessed client-side (runner/packages/af-control's own comment on the same
-- rule).
SELECT COALESCE(MAX(seq), -1)::bigint AS high_water_seq
FROM event WHERE run_id = sqlc.arg(run_id)::uuid AND source = 'dsh';

-- name: ListEventsSince :many
-- GET /v1/events?since= (SSE, P4). (at, id) is the total read order —
-- event_at_id_idx.
SELECT * FROM event WHERE at > $1 ORDER BY at, id LIMIT $2;

-- name: ListEventsByTask :many
-- event.task_id is nullable (an event can be scoped to a run only); the
-- explicit ::uuid cast is what makes sqlc emit uuid.UUID here instead of
-- pgtype.UUID for the PARAMETER — verified live, sqlc's inference defaults
-- a bare `WHERE nullable_col = $1` parameter to pgtype.UUID even though the
-- overrides.go_type override applies correctly everywhere the cast is
-- explicit. The column itself stays a plain uuid.UUID in the RETURNING/
-- SELECT * results either way; this only affects the query's own argument.
SELECT * FROM event WHERE task_id = sqlc.arg(task_id)::uuid ORDER BY at;
