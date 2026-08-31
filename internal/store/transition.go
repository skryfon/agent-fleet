package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/domain"
	"agentfleet/internal/redact"
	db "agentfleet/internal/store/gen"
)

// TransitionRequest is what a caller (internal/api's transition handlers,
// P4) hands to ApplyTaskTransition. It carries no storage-layer details
// (seq, event id, outbox id) — those are allocated inside the transaction.
type TransitionRequest struct {
	TaskID  uuid.UUID
	RunID   *uuid.UUID // nil for task-only triggers (e.g. TrIngested)
	Trigger domain.Trigger
	Actor   string
	Payload map[string]any
	// DedupeKey, if set, dedupes the control-plane EVENT this transition
	// writes (event_dedupe_uk) — distinct from each scheduled effect's own
	// outbox key. Typically a client-supplied Idempotency-Key header, e.g.
	// on POST /v1/tasks/{id}/start.
	DedupeKey *string
}

// TransitionResult reports what ApplyTaskTransition actually did, for the
// HTTP handler's response body and for tests.
type TransitionResult struct {
	From, To  domain.TaskState
	EventID   int64
	OutboxIDs []int64
}

func uuidParam(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func nullableUUIDParam(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}

	return uuidParam(*id)
}

// effectPayload is the JSON body every scheduled outbox row carries — enough
// context for internal/outbox's registered handlers (run.launch, run.kill,
// zulip.notify) to act without a second database round trip.
type effectPayload struct {
	TaskID string `json:"task_id"`
	RunID  string `json:"run_id,omitempty"`
}

// ApplyTaskTransition is the single atomic commit point docs/adr/0010's
// durability claim rests on: SELECT ... FOR UPDATE (serializes concurrent
// triggers against this task), domain.NextTask (pure — errors roll back the
// whole transaction, never a partial write), UPDATE task, INSERT event,
// INSERT outbox rows — all in one Store.WithTx call. Either all of it lands,
// or none of it does.
//
// An ErrIllegalTransition from domain.NextTask is returned unwrapped (via
// WithTx's error passthrough), so callers can errors.Is against it directly
// to render a 409 rather than a 500.
func (s *Store) ApplyTaskTransition(ctx context.Context, r *redact.Redactor, req TransitionRequest) (TransitionResult, error) {
	var result TransitionResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, req.TaskID)
		if err != nil {
			return fmt.Errorf("store: loading task %s for update: %w", req.TaskID, err)
		}

		tc := domain.TransitionContext{
			TaskID:      req.TaskID.String(),
			RequestedBy: req.Actor,
			Payload:     req.Payload,
		}
		if req.RunID != nil {
			tc.RunID = req.RunID.String()
		}

		from := domain.TaskState(task.State)

		outcome, err := domain.NextTask(from, req.Trigger, tc)
		if err != nil {
			// domain.ErrIllegalTransition (or a wrap of it) — returned as-is
			// so the caller's errors.Is check still works after WithTx's
			// passthrough.
			return err
		}

		if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{
			ID:    req.TaskID,
			State: string(outcome.To),
		}); err != nil {
			return fmt.Errorf("store: updating task state: %w", err)
		}

		payloadJSON, err := json.Marshal(outcome.Event.Payload)
		if err != nil {
			return fmt.Errorf("store: marshaling event payload: %w", err)
		}

		redacted, err := r.JSON(payloadJSON)
		if err != nil {
			return fmt.Errorf("store: redacting event payload: %w", err)
		}

		ev, err := q.InsertControlPlaneEvent(ctx, db.InsertControlPlaneEventParams{
			RunID:     nullableUUIDParam(req.RunID),
			TaskID:    uuidParam(req.TaskID),
			Seq:       task.NextEventSeq,
			Kind:      outcome.Event.Kind,
			Actor:     outcome.Event.Actor,
			Payload:   redacted,
			DedupeKey: req.DedupeKey,
		})
		if err != nil {
			return fmt.Errorf("store: inserting transition event: %w", err)
		}

		effPayload := effectPayload{TaskID: req.TaskID.String()}
		if req.RunID != nil {
			effPayload.RunID = req.RunID.String()
		}

		effPayloadJSON, err := json.Marshal(effPayload)
		if err != nil {
			return fmt.Errorf("store: marshaling effect payload: %w", err)
		}

		outboxIDs := make([]int64, 0, len(outcome.Effects))

		for _, eff := range outcome.Effects {
			key := eff.Key

			row, err := q.EnqueueOutbox(ctx, db.EnqueueOutboxParams{
				Topic:   eff.Topic,
				Payload: effPayloadJSON,
				Key:     &key,
			})
			// EnqueueOutbox returns no row, i.e. pgx.ErrNoRows, when
			// ON CONFLICT (key) ... DO NOTHING fires (this effect's key was
			// already enqueued by an earlier attempt at this same
			// transition) — verified live while writing 0002_control_plane's
			// outbox_key_uk. That is success, not failure: the effect is
			// enqueued exactly once either way. Only a REAL error (not
			// no-rows) rolls back the transaction.
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("store: enqueueing outbox effect %s: %w", eff.Topic, err)
			}

			if err == nil {
				outboxIDs = append(outboxIDs, row.ID)
			}
		}

		result = TransitionResult{From: from, To: outcome.To, EventID: ev.ID, OutboxIDs: outboxIDs}

		return nil
	})

	return result, err
}

// RunTransitionRequest mirrors TransitionRequest for run.state.
type RunTransitionRequest struct {
	RunID     uuid.UUID
	Trigger   domain.Trigger
	Actor     string
	Payload   map[string]any
	DedupeKey *string
}

// RunTransitionResult mirrors TransitionResult for run.state.
type RunTransitionResult struct {
	From, To domain.RunState
	EventID  int64
}

// ApplyRunTransition is ApplyTaskTransition's counterpart for run.state — a
// run transition never itself schedules an outbox effect (see
// domain.RunOutcome's doc comment); the task-level transition that observes
// the run's outcome is what schedules follow-on effects.
func (s *Store) ApplyRunTransition(ctx context.Context, r *redact.Redactor, req RunTransitionRequest) (RunTransitionResult, error) {
	var result RunTransitionResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		run, err := q.GetRunForUpdate(ctx, req.RunID)
		if err != nil {
			return fmt.Errorf("store: loading run %s for update: %w", req.RunID, err)
		}

		tc := domain.TransitionContext{RunID: req.RunID.String(), RequestedBy: req.Actor, Payload: req.Payload}
		from := domain.RunState(run.State)

		outcome, err := domain.NextRun(from, req.Trigger, tc)
		if err != nil {
			return err
		}

		if _, err := q.UpdateRunState(ctx, db.UpdateRunStateParams{ID: req.RunID, State: string(outcome.To)}); err != nil {
			return fmt.Errorf("store: updating run state: %w", err)
		}

		payloadJSON, err := json.Marshal(outcome.Event.Payload)
		if err != nil {
			return fmt.Errorf("store: marshaling event payload: %w", err)
		}

		redacted, err := r.JSON(payloadJSON)
		if err != nil {
			return fmt.Errorf("store: redacting event payload: %w", err)
		}

		ev, err := q.InsertControlPlaneEvent(ctx, db.InsertControlPlaneEventParams{
			RunID:     uuidParam(req.RunID),
			TaskID:    uuidParam(run.TaskID),
			Seq:       run.NextEventSeq,
			Kind:      outcome.Event.Kind,
			Actor:     outcome.Event.Actor,
			Payload:   redacted,
			DedupeKey: req.DedupeKey,
		})
		if err != nil {
			return fmt.Errorf("store: inserting run transition event: %w", err)
		}

		result = RunTransitionResult{From: from, To: outcome.To, EventID: ev.ID}

		return nil
	})

	return result, err
}
