package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/domain"
	"agentfleet/internal/redact"
	db "agentfleet/internal/store/gen"
)

// MirrorEvent is one dsh SessionEvent, as af-control's batch POST hands it
// to the control-plane API (P4). seq is NEVER renumbered — it is dsh's own
// SessionEvent.seq, stable across replays; that stability is what makes
// AppendMirror's ON CONFLICT DO NOTHING a true no-op on retry (docs/adr/0001).
type MirrorEvent struct {
	Seq     int64
	Kind    string
	Actor   string
	Payload []byte // raw JSON, redacted by AppendMirror before it ever reaches Postgres
	At      time.Time
}

// MirrorBatch is one POST /v1/runs/{id}/events request's worth of events.
type MirrorBatch struct {
	RunID  uuid.UUID
	TaskID uuid.UUID
	Events []MirrorEvent
}

// MirrorResult reports what AppendMirror actually did, for the handler's
// response body (af-control uses Accepted/Duplicates to decide what it can
// safely trim from its local retry buffer).
type MirrorResult struct {
	Accepted   int
	Duplicates int
}

// AppendMirror is the ONLY sanctioned entry point for writing
// source='dsh' event rows. It redacts every payload in the batch before
// the insert — internal/redact's package doc names this method exactly
// because it is the choke point secrets must pass through
// (development-plan.md §8: "redaction filter applies to every emitted
// event").
//
// Nothing else in this codebase may call db.Queries.AppendMirrorEvents (the
// sqlc-generated, unredacted raw query) directly — that is a code-review
// discipline, not a compiler-enforced one (Go's `internal/store/gen`
// package must stay public for sqlc's own generation model), the same way
// `event`'s append-only-ness is guarded by both a Postgres trigger AND
// convention. If a caller ever needs a source='dsh' write path, it should
// gain a new *exported* internal/store method that also redacts, not a
// direct sqlc.Queries call.
func (s *Store) AppendMirror(ctx context.Context, r *redact.Redactor, batch MirrorBatch) (MirrorResult, error) {
	if len(batch.Events) == 0 {
		return MirrorResult{}, nil
	}

	params := db.AppendMirrorEventsParams{
		RunID:    batch.RunID,
		TaskID:   batch.TaskID,
		Seqs:     make([]int64, len(batch.Events)),
		Kinds:    make([]string, len(batch.Events)),
		Actors:   make([]string, len(batch.Events)),
		Payloads: make([][]byte, len(batch.Events)),
		Ats:      make([]pgtype.Timestamptz, len(batch.Events)),
	}

	for i, ev := range batch.Events {
		redacted, err := r.JSON(ev.Payload)
		if err != nil {
			return MirrorResult{}, fmt.Errorf("store: redacting mirror event seq=%d: %w", ev.Seq, err)
		}

		params.Seqs[i] = ev.Seq
		params.Kinds[i] = ev.Kind
		params.Actors[i] = ev.Actor
		params.Payloads[i] = redacted
		params.Ats[i] = pgtype.Timestamptz{Time: ev.At, Valid: true}
	}

	accepted, err := s.q.AppendMirrorEvents(ctx, params)
	if err != nil {
		return MirrorResult{}, fmt.Errorf("store: appending mirror batch: %w", err)
	}

	return MirrorResult{
		Accepted:   int(accepted),
		Duplicates: len(batch.Events) - int(accepted),
	}, nil
}

// MirrorHighWaterSeq is the value af-control seeds its client-side
// highWaterSeq from on (re)connect — never guessed client-side.
func (s *Store) MirrorHighWaterSeq(ctx context.Context, runID uuid.UUID) (int64, error) {
	return s.q.MirrorHighWaterSeq(ctx, runID)
}

// RecordEvent inserts a source='control_plane' event scoped to runID (and
// its task) with NO accompanying state transition — for facts worth
// auditing that are not themselves a task/run lifecycle change, chiefly
// internal/api's mediated tool-dispatch policy decision (development-plan.md
// §4: "the decision is recorded as an event" before the effect, if any,
// runs). Still redacts (this package's whole reason to exist) and still
// allocates seq under GetRunForUpdate's row lock, via the new
// IncrementRunEventSeq query, so seq numbers never race with a concurrent
// ApplyRunTransition/ApplyRunExit on the same run.
//
// dedupeKey mirrors TransitionRequest.DedupeKey (event_dedupe_uk) — flagged
// in DB review: unlike every transition-writing path, this method
// originally had no way to dedupe a client-side retry (e.g. a tool-dispatch
// client retrying a POST whose response was lost), which would otherwise
// mint a second distinct event via a fresh IncrementRunEventSeq call
// instead of a no-op. Pass nil when the caller has no natural idempotency
// key of its own.
func (s *Store) RecordEvent(ctx context.Context, r *redact.Redactor, runID uuid.UUID, kind string, payload map[string]any, dedupeKey *string) (db.Event, error) {
	var ev db.Event

	err := s.WithTx(ctx, func(q *db.Queries) error {
		run, err := q.GetRunForUpdate(ctx, runID)
		if err != nil {
			return fmt.Errorf("store: loading run %s for update: %w", runID, err)
		}

		if _, err := q.IncrementRunEventSeq(ctx, runID); err != nil {
			return fmt.Errorf("store: bumping run event seq: %w", err)
		}

		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("store: marshaling event payload: %w", err)
		}

		redacted, err := r.JSON(payloadJSON)
		if err != nil {
			return fmt.Errorf("store: redacting event payload: %w", err)
		}

		row, err := q.InsertControlPlaneEvent(ctx, db.InsertControlPlaneEventParams{
			RunID:     uuidParam(runID),
			TaskID:    uuidParam(run.TaskID),
			Seq:       run.NextEventSeq,
			Kind:      kind,
			Actor:     domain.ActorControlPlane,
			Payload:   redacted,
			DedupeKey: dedupeKey,
		})
		if err != nil {
			return fmt.Errorf("store: inserting event: %w", err)
		}

		ev = row

		return nil
	})

	return ev, err
}
