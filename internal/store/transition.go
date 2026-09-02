package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/domain"
	"agentfleet/internal/redact"
	db "agentfleet/internal/store/gen"
)

// ErrQuestionAlreadyOpen is ApplyAsk's sentinel for a
// question_one_open_per_feature_uk violation — "one open question per topic
// at a time" (development-plan.md §6). internal/api's ask_human handler
// turns this into a 409 telling the caller to wait, not a 500.
var ErrQuestionAlreadyOpen = errors.New("store: a question is already open for this feature")

// isUniqueViolation reports whether err is a Postgres unique_violation
// (SQLSTATE 23505) against the named constraint — used to translate a
// racing INSERT into a documented sentinel rather than a bare 500.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

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

// numericParam converts a plain float64 (a request body's cost_usd/usd_cap,
// never itself a stored value on its own) to the pgtype.Numeric a `numeric`
// column expects. Scan(string) is pgtype.Numeric's own documented decode
// path — the same one Postgres text-protocol results already go through —
// so this stays exact for the 4-decimal-place amounts development-plan.md
// §6's budgets deal in, unlike a binary float64->Numeric conversion would.
func numericParam(v float64) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(strconv.FormatFloat(v, 'f', 4, 64))

	return n
}

// float64FromNumeric is numericParam's inverse, for reading a budget row's
// spent/cap columns back out to evaluate against internal/budget.Caps/Spent
// (plain float64 — that package has no reason to know about pgtype).
func float64FromNumeric(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}

	return f.Float64
}

// effectPayload is the JSON body every scheduled outbox row carries — enough
// context for internal/outbox's registered handlers (run.launch, run.kill,
// zulip.question/zulip.review/zulip.failed) to act without a second database
// round trip. Duplicated (deliberately, per the original P3 comment) in
// internal/supervisor/handlers.go — that copy only reads TaskID, so adding
// QuestionID here does not require touching it.
type effectPayload struct {
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
}

// insertTransitionEvent is the marshal -> redact -> insert sequence shared
// by every transition path (ApplyTaskTransition, ApplyRunTransition,
// ApplyRunExit's two writes) — factored out so a fix to this sequence (the
// redaction call, the error-wrapping) happens once, not once per call site.
func insertTransitionEvent(
	ctx context.Context, q *db.Queries, r *redact.Redactor,
	runID pgtype.UUID, taskID pgtype.UUID, seq int64, ev domain.PendingEvent, dedupeKey *string,
) (db.Event, error) {
	payloadJSON, err := json.Marshal(ev.Payload)
	if err != nil {
		return db.Event{}, fmt.Errorf("store: marshaling event payload: %w", err)
	}

	redacted, err := r.JSON(payloadJSON)
	if err != nil {
		return db.Event{}, fmt.Errorf("store: redacting event payload: %w", err)
	}

	row, err := q.InsertControlPlaneEvent(ctx, db.InsertControlPlaneEventParams{
		RunID:     runID,
		TaskID:    taskID,
		Seq:       seq,
		Kind:      ev.Kind,
		Actor:     ev.Actor,
		Payload:   redacted,
		DedupeKey: dedupeKey,
	})
	if err != nil {
		return db.Event{}, fmt.Errorf("store: inserting transition event: %w", err)
	}

	return row, nil
}

// enqueueEffects enqueues one outbox row per domain.PendingEffect, tolerating
// (as success) the ON CONFLICT (key) ... DO NOTHING case — see the comment
// inline below. eventID is the just-inserted transition event's own id
// (from insertTransitionEvent) — see domain.EffectSpec.KeyReason's doc
// comment for why the key is composed here, from the event id, rather than
// rendered from a template inside internal/domain: at the point NextTask
// runs, the event doesn't exist yet (its id is a Postgres-allocated
// bigserial, only known after the INSERT).
func enqueueEffects(ctx context.Context, q *db.Queries, effects []domain.PendingEffect, eventID int64, payload []byte) ([]int64, error) {
	outboxIDs := make([]int64, 0, len(effects))

	for _, eff := range effects {
		key := fmt.Sprintf("%s:%d", eff.KeyReason, eventID)

		row, err := q.EnqueueOutbox(ctx, db.EnqueueOutboxParams{
			Topic:   eff.Topic,
			Payload: payload,
			Key:     &key,
		})
		// EnqueueOutbox returns no row, i.e. pgx.ErrNoRows, when
		// ON CONFLICT (key) ... DO NOTHING fires (this effect's key was
		// already enqueued by an earlier attempt at this same transition) —
		// verified live while writing 0002_control_plane's outbox_key_uk.
		// That is success, not failure: the effect is enqueued exactly once
		// either way. Only a REAL error (not no-rows) rolls back the
		// transaction.
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("store: enqueueing outbox effect %s: %w", eff.Topic, err)
		}

		if err == nil {
			outboxIDs = append(outboxIDs, row.ID)
		}
	}

	return outboxIDs, nil
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
//
// This method does NOT also lock/transition a run row — for the one caller
// that needs both (a run exiting, which reacts on both run.state AND
// task.state), use ApplyRunExit instead, which does both in one transaction.
// Calling ApplyRunTransition and ApplyTaskTransition back-to-back as two
// separate calls is NOT atomic and must never be used for a run-exit
// reaction — caught in code review before any caller did this.
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

		ev, err := insertTransitionEvent(ctx, q, r, nullableUUIDParam(req.RunID), uuidParam(req.TaskID), task.NextEventSeq, outcome.Event, req.DedupeKey)
		if err != nil {
			return err
		}

		effPayload := effectPayload{TaskID: req.TaskID.String()}
		if req.RunID != nil {
			effPayload.RunID = req.RunID.String()
		}

		effPayloadJSON, err := json.Marshal(effPayload)
		if err != nil {
			return fmt.Errorf("store: marshaling effect payload: %w", err)
		}

		outboxIDs, err := enqueueEffects(ctx, q, outcome.Effects, ev.ID, effPayloadJSON)
		if err != nil {
			return err
		}

		result = TransitionResult{From: from, To: outcome.To, EventID: ev.ID, OutboxIDs: outboxIDs}

		return nil
	})

	return result, err
}

// AskRequest is what internal/api's ask_human handler hands to ApplyAsk —
// enough to insert the question row and fire TrAsked in the same
// transaction as ApplyTaskTransition's own state-change/event/outbox write.
type AskRequest struct {
	RunID     uuid.UUID
	TaskID    uuid.UUID
	Kind      string // 'choice' | 'confirm' | 'free_text' (question_kind_ck)
	Body      string
	Options   []byte // jsonb; nil defaults to '[]'
	Addressee *string
	Actor     string
	DedupeKey *string
}

// AskResult reports the created question alongside the task transition it
// caused.
type AskResult struct {
	Question db.Question
	Task     TransitionResult
}

// ApplyAsk is ApplyTaskTransition's counterpart for TrAsked: it locks the
// task, inserts the question row (feature_id copied from the locked task
// row, so question_one_open_per_feature_uk enforces "one open question per
// topic at a time" without a second query), applies TrAsked, and enqueues
// the zulip.question effect — all in one WithTx, the same atomicity
// ApplyTaskTransition's own doc comment describes.
//
// A concurrent ask against the same feature loses the race at the INSERT's
// unique_violation and this returns ErrQuestionAlreadyOpen, unwrapped, so
// callers can errors.Is against it (same convention as
// domain.ErrIllegalTransition).
func (s *Store) ApplyAsk(ctx context.Context, r *redact.Redactor, req AskRequest) (AskResult, error) {
	var result AskResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		task, err := q.GetTaskForUpdate(ctx, req.TaskID)
		if err != nil {
			return fmt.Errorf("store: loading task %s for update: %w", req.TaskID, err)
		}

		options := req.Options
		if options == nil {
			options = []byte(`[]`)
		}

		question, err := q.CreateQuestion(ctx, db.CreateQuestionParams{
			RunID:     req.RunID,
			TaskID:    req.TaskID,
			FeatureID: uuidParam(task.FeatureID),
			Kind:      req.Kind,
			Body:      req.Body,
			Options:   options,
			Addressee: req.Addressee,
			State:     "OPEN",
		})
		if err != nil {
			if isUniqueViolation(err, "question_one_open_per_feature_uk") {
				return ErrQuestionAlreadyOpen
			}

			return fmt.Errorf("store: creating question: %w", err)
		}

		tc := domain.TransitionContext{
			TaskID:      req.TaskID.String(),
			RunID:       req.RunID.String(),
			QuestionID:  question.ID.String(),
			RequestedBy: req.Actor,
		}

		from := domain.TaskState(task.State)

		outcome, err := domain.NextTask(from, domain.TrAsked, tc)
		if err != nil {
			return err
		}

		if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{
			ID:    req.TaskID,
			State: string(outcome.To),
		}); err != nil {
			return fmt.Errorf("store: updating task state: %w", err)
		}

		ev, err := insertTransitionEvent(ctx, q, r, uuidParam(req.RunID), uuidParam(req.TaskID), task.NextEventSeq, outcome.Event, req.DedupeKey)
		if err != nil {
			return err
		}

		effPayloadJSON, err := json.Marshal(effectPayload{
			TaskID:     req.TaskID.String(),
			RunID:      req.RunID.String(),
			QuestionID: question.ID.String(),
		})
		if err != nil {
			return fmt.Errorf("store: marshaling effect payload: %w", err)
		}

		outboxIDs, err := enqueueEffects(ctx, q, outcome.Effects, ev.ID, effPayloadJSON)
		if err != nil {
			return err
		}

		result = AskResult{
			Question: question,
			Task:     TransitionResult{From: from, To: outcome.To, EventID: ev.ID, OutboxIDs: outboxIDs},
		}

		return nil
	})

	return result, err
}

// AnswerRequest is what internal/api's answerQuestion handler hands to
// ApplyAnswer — the counterpart write for TrAnswered.
type AnswerRequest struct {
	QuestionID uuid.UUID
	Answer     string
	AnsweredBy string
	Actor      string
	DedupeKey  *string
}

// AnswerResult reports the answered question alongside the task transition
// it caused. Task.OutboxIDs carries the run.launch/resume effect's outbox
// row id — internal/supervisor.RunLaunch is what actually resurrects the
// container.
type AnswerResult struct {
	Question db.Question
	Task     TransitionResult
}

// ErrQuestionNotOpen is ApplyAnswer's sentinel for answering a question
// that isn't OPEN (already answered, timed out, or cancelled) — a 409, not
// a 500: two humans racing to answer the same question, or a stale Zulip
// reply arriving after a timeout parked the task.
var ErrQuestionNotOpen = errors.New("store: question is not open")

// ApplyAnswer is ApplyAsk's counterpart for TrAnswered: locks the question's
// task, flips the question to ANSWERED (AnswerQuestion's own WHERE
// state='OPEN' guard is what ErrQuestionNotOpen surfaces), applies
// TrAnswered, and enqueues the run.launch/resume effect — one WithTx.
func (s *Store) ApplyAnswer(ctx context.Context, r *redact.Redactor, req AnswerRequest) (AnswerResult, error) {
	var result AnswerResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		question, err := q.GetQuestionForUpdate(ctx, req.QuestionID)
		if err != nil {
			return fmt.Errorf("store: loading question %s for update: %w", req.QuestionID, err)
		}

		if question.State != "OPEN" {
			return ErrQuestionNotOpen
		}

		question, err = q.AnswerQuestion(ctx, db.AnswerQuestionParams{
			ID:         req.QuestionID,
			Answer:     &req.Answer,
			AnsweredBy: &req.AnsweredBy,
		})
		if err != nil {
			// A concurrent answer between the FOR-UPDATE read above and this
			// UPDATE is impossible (the lock is held for the transaction's
			// duration), so a no-rows result here would indicate a real bug,
			// not a race — surfaced as a plain error, not ErrQuestionNotOpen.
			return fmt.Errorf("store: answering question %s: %w", req.QuestionID, err)
		}

		task, err := q.GetTaskForUpdate(ctx, question.TaskID)
		if err != nil {
			return fmt.Errorf("store: loading task %s for update: %w", question.TaskID, err)
		}

		tc := domain.TransitionContext{
			TaskID:      question.TaskID.String(),
			RunID:       question.RunID.String(),
			QuestionID:  question.ID.String(),
			RequestedBy: req.Actor,
		}

		from := domain.TaskState(task.State)

		outcome, err := domain.NextTask(from, domain.TrAnswered, tc)
		if err != nil {
			return err
		}

		if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{
			ID:    question.TaskID,
			State: string(outcome.To),
		}); err != nil {
			return fmt.Errorf("store: updating task state: %w", err)
		}

		ev, err := insertTransitionEvent(ctx, q, r, uuidParam(question.RunID), uuidParam(question.TaskID), task.NextEventSeq, outcome.Event, req.DedupeKey)
		if err != nil {
			return err
		}

		// RunID carries the JUST-EXITED run's id (per
		// internal/supervisor.effectPayload's own doc comment: "a
		// resurrect-and-resume launch carries an existing run's id") — this
		// is what lets RunLaunch tell a resume launch from a first launch and
		// pull that run's dsh_session_id forward (Phase 4).
		effPayloadJSON, err := json.Marshal(effectPayload{
			TaskID:     question.TaskID.String(),
			RunID:      question.RunID.String(),
			QuestionID: question.ID.String(),
		})
		if err != nil {
			return fmt.Errorf("store: marshaling effect payload: %w", err)
		}

		outboxIDs, err := enqueueEffects(ctx, q, outcome.Effects, ev.ID, effPayloadJSON)
		if err != nil {
			return err
		}

		result = AnswerResult{
			Question: question,
			Task:     TransitionResult{From: from, To: outcome.To, EventID: ev.ID, OutboxIDs: outboxIDs},
		}

		return nil
	})

	return result, err
}

// ErrApprovalArtifactNotFound is ApplyApproval's sentinel when subject_ref
// names no known artifact — a 404, not a 500.
var ErrApprovalArtifactNotFound = errors.New("store: no artifact found for subject_ref")

// ErrApprovalSHAMismatch is ApplyApproval's sentinel for
// development-plan.md §3's own invariant: "a revised artifact voids its
// approval." internal/api's approvals handler turns this into a 409.
var ErrApprovalSHAMismatch = errors.New("store: approval sha256 does not match the current artifact")

// ApprovalRequest is what internal/api's approvals handler hands to
// ApplyApproval.
type ApprovalRequest struct {
	SubjectKind string // 'spec' | 'plan' | 'pr' (artifact_kind_ck/approval_subject_kind_ck)
	SubjectRef  string // the artifact's uri
	Sha256      string
	Decision    string // 'APPROVED' | 'REJECTED' (approval_decision_ck)
	Actor       string
	Note        *string
	DedupeKey   *string
}

// ApprovalResult reports the recorded approval alongside the task
// transition it caused.
type ApprovalResult struct {
	Approval db.Approval
	Task     TransitionResult
}

// ApplyApproval is ApplyAsk's counterpart for the REVIEW gate
// (development-plan.md §4 M4): resolve the artifact subject_ref names,
// reject with ErrApprovalSHAMismatch if req.Sha256 doesn't match its
// CURRENT sha256 (the artifact may have been re-opened since the human
// last looked at it), insert the approval row, and drive TrApproved
// (APPROVED) or TrCancel (REJECTED) — all in one WithTx, same atomicity as
// every other Apply* method in this file.
func (s *Store) ApplyApproval(ctx context.Context, r *redact.Redactor, req ApprovalRequest) (ApprovalResult, error) {
	var result ApprovalResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		artifact, err := q.GetArtifactByURI(ctx, req.SubjectRef)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrApprovalArtifactNotFound
			}

			return fmt.Errorf("store: loading artifact %s: %w", req.SubjectRef, err)
		}

		if artifact.Sha256 != req.Sha256 {
			return ErrApprovalSHAMismatch
		}

		approval, err := q.InsertApproval(ctx, db.InsertApprovalParams{
			SubjectKind: req.SubjectKind, SubjectRef: req.SubjectRef, SubjectSha256: req.Sha256,
			Decision: req.Decision, Actor: req.Actor, Note: req.Note,
		})
		if err != nil {
			return fmt.Errorf("store: inserting approval: %w", err)
		}

		tr := domain.TrApproved
		if req.Decision == "REJECTED" {
			tr = domain.TrCancel
		}

		task, err := q.GetTaskForUpdate(ctx, artifact.TaskID)
		if err != nil {
			return fmt.Errorf("store: loading task %s for update: %w", artifact.TaskID, err)
		}

		tc := domain.TransitionContext{TaskID: artifact.TaskID.String(), RequestedBy: req.Actor}
		from := domain.TaskState(task.State)

		outcome, err := domain.NextTask(from, tr, tc)
		if err != nil {
			return err
		}

		if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{
			ID: artifact.TaskID, State: string(outcome.To),
		}); err != nil {
			return fmt.Errorf("store: updating task state: %w", err)
		}

		ev, err := insertTransitionEvent(ctx, q, r, pgtype.UUID{}, uuidParam(artifact.TaskID), task.NextEventSeq, outcome.Event, req.DedupeKey)
		if err != nil {
			return err
		}

		effPayloadJSON, err := json.Marshal(effectPayload{TaskID: artifact.TaskID.String()})
		if err != nil {
			return fmt.Errorf("store: marshaling effect payload: %w", err)
		}

		outboxIDs, err := enqueueEffects(ctx, q, outcome.Effects, ev.ID, effPayloadJSON)
		if err != nil {
			return err
		}

		result = ApprovalResult{
			Approval: approval,
			Task:     TransitionResult{From: from, To: outcome.To, EventID: ev.ID, OutboxIDs: outboxIDs},
		}

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
	// ContainerID, when set, persists internal/supervisor's container id in
	// the SAME UPDATE (and therefore the same FOR-UPDATE-locked transaction)
	// as the state change, via SetRunContainerStarted instead of plain
	// UpdateRunState. Caught in DB review: writing this as a second,
	// separate statement after the transition committed left the row
	// unlocked in between, so a concurrent transition (e.g. TrCancel) could
	// legally land and then be silently clobbered when the second write
	// stamped State back to the transition's own To — a real lost-update
	// race, not just a missing-bookkeeping gap. Folding it into this one
	// transaction closes that.
	ContainerID *string
	// ExitCode, when set, persists the container's exit code in the SAME
	// UPDATE as the state change, via SetRunExited instead of plain
	// UpdateRunState — mutually exclusive with ContainerID (a run reports
	// either "started" or "exited", never both in one call). M3's
	// checkpoint-and-exit is this field's own reason to exist: a run whose
	// TASK already left RUNNING for BLOCKED_ON_HUMAN (Store.ApplyAsk, before
	// the container ever exits) has no task-level reaction left to make when
	// its container later exits gracefully — see this method's own doc
	// comment on why that breaks the "a run exiting always has a task-level
	// reaction" assumption ApplyRunExit is built on.
	ExitCode *int32
}

// RunTransitionResult mirrors TransitionResult for run.state.
type RunTransitionResult struct {
	From, To domain.RunState
	EventID  int64
}

// ApplyRunTransition is ApplyTaskTransition's counterpart for run.state,
// for run-only triggers that have no task-level reaction: TrRunStarted
// (PENDING->STARTING->RUNNING as the supervisor confirms container
// lifecycle), TrCancel from a not-yet-running run, and — as of M3 — a run
// EXITING whose task is BLOCKED_ON_HUMAN (the task already reacted, via
// Store.ApplyAsk, before this run's container even stopped; nothing is left
// for a task-level trigger to do). Every OTHER run exit still has a
// task-level reaction and must use ApplyRunExit instead of this method
// followed by a separate ApplyTaskTransition call (see ApplyTaskTransition's
// doc comment) — internal/api's reportContainerExited is what decides which
// method applies, by checking the task's current state first.
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

		switch {
		case req.ContainerID != nil:
			if _, err := q.SetRunContainerStarted(ctx, db.SetRunContainerStartedParams{
				ID: req.RunID, State: string(outcome.To), ContainerID: req.ContainerID,
			}); err != nil {
				return fmt.Errorf("store: setting run container started: %w", err)
			}
		case req.ExitCode != nil:
			if _, err := q.SetRunExited(ctx, db.SetRunExitedParams{
				ID: req.RunID, State: string(outcome.To), ExitCode: req.ExitCode,
			}); err != nil {
				return fmt.Errorf("store: setting run exited: %w", err)
			}
		default:
			if _, err := q.UpdateRunState(ctx, db.UpdateRunStateParams{ID: req.RunID, State: string(outcome.To)}); err != nil {
				return fmt.Errorf("store: updating run state: %w", err)
			}
		}

		ev, err := insertTransitionEvent(ctx, q, r, uuidParam(req.RunID), uuidParam(run.TaskID), run.NextEventSeq, outcome.Event, req.DedupeKey)
		if err != nil {
			return err
		}

		result = RunTransitionResult{From: from, To: outcome.To, EventID: ev.ID}

		return nil
	})

	return result, err
}

// RunExitRequest is what a caller hands to ApplyRunExit when a run has
// finished (successfully or not) and both run.state and task.state must
// react — the one case docs/adr/0010's atomicity claim would otherwise be
// silently violated by two separate transactions (caught in code review).
type RunExitRequest struct {
	RunID uuid.UUID
	// RunTrigger is TrRunExitedOK or TrRunFailed — see internal/domain's
	// runTable.
	RunTrigger domain.Trigger
	// TaskTrigger is TrRunExitedOK, TrRunExitedErrRetryable, or
	// TrRunExitedErrFinal — the caller (P4's run-exit handler, or
	// internal/reconcile) has already decided retryable-vs-final by
	// comparing task.attempt against the configured max BEFORE calling
	// this; see internal/domain/state.go's doc comment on those two
	// triggers, and 0002_control_plane.up.sql's sourcing comment on
	// task.attempt for why it is the task's counter, not the exiting run's
	// own (a retry gets a brand-new run row, whose run.attempt column
	// starts back at 0 — caught in code review as a cap that would
	// otherwise never actually cap anything once P5 starts creating real
	// run rows).
	TaskTrigger domain.Trigger
	Actor       string
	Payload     map[string]any
	// ExitCode, when set, persists the container's exit code in the SAME
	// UPDATE as the run-state change (via SetRunExited instead of plain
	// UpdateRunState) — see RunTransitionRequest.ContainerID's doc comment
	// for why a second, separate statement after commit is a real race, not
	// just missing bookkeeping.
	ExitCode *int32
	// IncrementAttempt, when true, bumps task.attempt in this same
	// transaction (under the same GetTaskForUpdate lock ApplyRunExit's own
	// UpdateTaskState call uses) — the caller has already decided
	// TaskTrigger is TrRunExitedErrRetryable based on the PRE-increment
	// attempt count; folding the bump in here keeps that decision and its
	// consequence atomic with each other instead of a separate post-commit
	// call that could be lost to a crash.
	IncrementAttempt bool
}

// RunExitResult reports both halves of what ApplyRunExit did.
type RunExitResult struct {
	Run  RunTransitionResult
	Task TransitionResult
}

// ApplyRunExit locks and transitions BOTH the run row and its task row, in
// that order, inside ONE transaction: run.state reacts first (RunTrigger),
// then task.state reacts to the run's outcome (TaskTrigger), then the
// task-level outbox effects (e.g. zulip.notify) enqueue — all atomically.
// This is the method a run-exit callback (the supervisor reporting a
// container exit, or internal/reconcile re-driving a stale run) must use;
// calling ApplyRunTransition and ApplyTaskTransition separately for the
// same exit is NOT atomic (see ApplyTaskTransition's doc comment).
//
// Lock order is run-then-task. ApplyTaskTransition never locks a run row,
// so there is no cross-method deadlock risk today — keep it that way if a
// future method needs both locks.
func (s *Store) ApplyRunExit(ctx context.Context, r *redact.Redactor, req RunExitRequest) (RunExitResult, error) {
	var result RunExitResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		run, err := q.GetRunForUpdate(ctx, req.RunID)
		if err != nil {
			return fmt.Errorf("store: loading run %s for update: %w", req.RunID, err)
		}

		runTC := domain.TransitionContext{RunID: req.RunID.String(), RequestedBy: req.Actor, Payload: req.Payload}
		runFrom := domain.RunState(run.State)

		runOutcome, err := domain.NextRun(runFrom, req.RunTrigger, runTC)
		if err != nil {
			return err
		}

		if req.ExitCode != nil {
			if _, err := q.SetRunExited(ctx, db.SetRunExitedParams{
				ID: req.RunID, State: string(runOutcome.To), ExitCode: req.ExitCode,
			}); err != nil {
				return fmt.Errorf("store: setting run exited: %w", err)
			}
		} else if _, err := q.UpdateRunState(ctx, db.UpdateRunStateParams{ID: req.RunID, State: string(runOutcome.To)}); err != nil {
			return fmt.Errorf("store: updating run state: %w", err)
		}

		runEv, err := insertTransitionEvent(ctx, q, r, uuidParam(req.RunID), uuidParam(run.TaskID), run.NextEventSeq, runOutcome.Event, nil)
		if err != nil {
			return err
		}

		task, err := q.GetTaskForUpdate(ctx, run.TaskID)
		if err != nil {
			return fmt.Errorf("store: loading task %s for update: %w", run.TaskID, err)
		}

		if req.IncrementAttempt {
			// task.attempt, not run.attempt — see RunExitRequest.IncrementAttempt's
			// doc comment. Bumped under the same GetTaskForUpdate lock the
			// UpdateTaskState call below uses, so this and the state change
			// commit together.
			if _, err := q.IncrementTaskAttempt(ctx, run.TaskID); err != nil {
				return fmt.Errorf("store: incrementing task attempt: %w", err)
			}
		}

		taskTC := domain.TransitionContext{
			TaskID:      run.TaskID.String(),
			RunID:       req.RunID.String(),
			RequestedBy: req.Actor,
			Payload:     req.Payload,
		}
		taskFrom := domain.TaskState(task.State)

		taskOutcome, err := domain.NextTask(taskFrom, req.TaskTrigger, taskTC)
		if err != nil {
			return err
		}

		if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{
			ID:    run.TaskID,
			State: string(taskOutcome.To),
		}); err != nil {
			return fmt.Errorf("store: updating task state: %w", err)
		}

		taskEv, err := insertTransitionEvent(ctx, q, r, uuidParam(req.RunID), uuidParam(run.TaskID), task.NextEventSeq, taskOutcome.Event, nil)
		if err != nil {
			return err
		}

		effPayloadJSON, err := json.Marshal(effectPayload{TaskID: run.TaskID.String(), RunID: req.RunID.String()})
		if err != nil {
			return fmt.Errorf("store: marshaling effect payload: %w", err)
		}

		outboxIDs, err := enqueueEffects(ctx, q, taskOutcome.Effects, taskEv.ID, effPayloadJSON)
		if err != nil {
			return err
		}

		result = RunExitResult{
			Run:  RunTransitionResult{From: runFrom, To: runOutcome.To, EventID: runEv.ID},
			Task: TransitionResult{From: taskFrom, To: taskOutcome.To, EventID: taskEv.ID, OutboxIDs: outboxIDs},
		}

		return nil
	})

	return result, err
}
