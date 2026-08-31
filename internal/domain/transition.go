package domain

import (
	"errors"
	"fmt"
	"maps"
	"strings"
)

// ErrIllegalTransition is returned by NextTask/NextRun when (From, Trigger)
// has no row in the table. Per development-plan.md §3 and docs/adr/0010:
// "Illegal transitions error, never silently no-op."
var ErrIllegalTransition = errors.New("domain: illegal state transition")

// EffectSpec names one outbound side effect a transition schedules.
// internal/store.ApplyTaskTransition inserts one outbox row per EffectSpec,
// in the same transaction as the state change and its event — that
// atomicity is docs/adr/0010's whole durability claim.
type EffectSpec struct {
	// Topic is the outbox row's topic; the relay dispatches by exact match
	// (internal/outbox.Relay.Handle).
	Topic string
	// KeyTemplate renders the outbox row's idempotency key. "{{run_id}}" and
	// "{{task_id}}" are substituted from TransitionContext by RenderKey.
	// A transition retried after a crash re-derives the identical key, so
	// the outbox's ON CONFLICT (key) ... DO NOTHING makes re-enqueueing a
	// no-op — see 0002_control_plane.up.sql's outbox_key_uk comment.
	KeyTemplate string
}

// RenderKey substitutes TransitionContext fields into a KeyTemplate.
func (e EffectSpec) RenderKey(tc TransitionContext) string {
	key := e.KeyTemplate
	key = strings.ReplaceAll(key, "{{run_id}}", tc.RunID)
	key = strings.ReplaceAll(key, "{{task_id}}", tc.TaskID)
	key = strings.ReplaceAll(key, "{{question_id}}", tc.QuestionID)

	return key
}

// TaskTransition is one row of the task state machine.
type TaskTransition struct {
	From      TaskState
	Trigger   Trigger
	To        TaskState
	EventKind string // written on the event row this transition emits, no exceptions
	Effects   []EffectSpec
}

// TransitionContext carries the identifiers and payload a transition needs
// to render effect keys and event payloads. It carries no IO handles, no
// clock, and no randomness — NextTask/NextRun stay pure functions of their
// arguments.
type TransitionContext struct {
	TaskID     string
	RunID      string
	QuestionID string
	// RequestedBy is the free-form identity that asked for this transition
	// (an API caller's identity, a container's run id, "reconciler") — for
	// the audit payload only. It is NOT the event.actor column: that column
	// is CHECK-constrained to a small fixed vocabulary
	// (0002_control_plane.up.sql's event_actor_ck) and every
	// control-plane-native event uniformly uses ActorControlPlane
	// regardless of who requested it — verified live: passing an arbitrary
	// caller identity into event.actor violates the constraint.
	RequestedBy string
	Payload     map[string]any
}

// ActorControlPlane is the only event.actor value NextTask/NextRun ever
// produce — see TransitionContext.RequestedBy's doc comment for why the
// requester's own identity does not belong in this column.
const ActorControlPlane = "control_plane"

// PendingEvent is the event row a transition will append. internal/store
// allocates the event's Postgres id and seq (from task.next_event_seq /
// run.next_event_seq) — NextTask only decides what kind/actor/payload to
// write, never the storage-layer identifiers.
type PendingEvent struct {
	Kind    string
	Actor   string
	Payload map[string]any
}

// eventPayload folds RequestedBy into the transition's own Payload without
// mutating the caller's map.
func eventPayload(tc TransitionContext) map[string]any {
	payload := make(map[string]any, len(tc.Payload)+1)
	maps.Copy(payload, tc.Payload)

	if tc.RequestedBy != "" {
		payload["requested_by"] = tc.RequestedBy
	}

	return payload
}

// PendingEffect is a rendered EffectSpec, ready for internal/store to insert
// as an outbox row.
type PendingEffect struct {
	Topic string
	Key   string
}

// Outcome is what a legal transition produces: the new state plus the event
// and effects internal/store must persist atomically with it.
type Outcome struct {
	To      TaskState
	Event   PendingEvent
	Effects []PendingEffect
}

// taskTable is the whole task lifecycle as one literal, in the order
// development-plan.md §3's diagram reads: the CREATED->QUEUED->RUNNING->
// REVIEW->DONE spine first, then the QUEUED<-RUNNING loop-back (a failed
// run requeues the task rather than failing it outright — retried, not
// abandoned), then RUNNING->BLOCKED_ON_HUMAN->RUNNING (§6's ask_human
// round-trip; TrAnswered lands in M3, the trigger and table row exist now
// so the state machine doesn't need a mid-M2/M3 schema change), then the
// three RUNNING-terminal branches, then REVIEW->DONE (§4's /v1/approvals,
// M4).
//
// This is the ONLY place task-lifecycle branching logic exists in this
// codebase (.claude/CLAUDE.md: "table-driven ... not scattered if chains").
// Editing it requires updating internal/domain/testdata/task_transitions.golden
// (go test ./internal/domain/... -run TestTaskTableGolden -update).
var taskTable = []TaskTransition{
	{
		From: TaskCreated, Trigger: TrIngested, To: TaskQueued,
		EventKind: "task_queued",
	},
	{
		From: TaskQueued, Trigger: TrStart, To: TaskRunning,
		EventKind: "task_started",
		Effects:   []EffectSpec{{Topic: "run.launch", KeyTemplate: "launch:{{run_id}}"}},
	},
	{
		From: TaskQueued, Trigger: TrCancel, To: TaskCancelled,
		EventKind: "task_cancelled",
	},
	{
		From: TaskRunning, Trigger: TrRunExitedOK, To: TaskReview,
		EventKind: "task_in_review",
		Effects:   []EffectSpec{{Topic: "zulip.notify", KeyTemplate: "review:{{task_id}}"}},
	},
	{
		// The QUEUED<-RUNNING loop-back the §3 diagram draws: a run that
		// exits in error requeues the task rather than failing it — a fresh
		// run gets a fresh attempt. The caller (supervisor's exit callback,
		// or internal/reconcile) is the one that decided this failure is
		// retryable (run.attempt below the configured max) before choosing
		// this trigger over TrRunExitedErrFinal.
		From: TaskRunning, Trigger: TrRunExitedErrRetryable, To: TaskQueued,
		EventKind: "task_requeued",
	},
	{
		// The §3 diagram's RUNNING -> FAILED branch: attempts exhausted,
		// the task will not be retried automatically. A human can still
		// re-queue it by hand later (out of scope for this table — that
		// would be a new TrIngested-shaped entry point, not a transition
		// from FAILED).
		From: TaskRunning, Trigger: TrRunExitedErrFinal, To: TaskFailed,
		EventKind: "task_failed",
		Effects:   []EffectSpec{{Topic: "zulip.notify", KeyTemplate: "failed:{{task_id}}"}},
	},
	{
		From: TaskRunning, Trigger: TrAsked, To: TaskBlockedOnHuman,
		EventKind: "task_blocked",
		Effects:   []EffectSpec{{Topic: "zulip.notify", KeyTemplate: "question:{{question_id}}"}},
	},
	{
		From: TaskBlockedOnHuman, Trigger: TrAnswered, To: TaskRunning,
		EventKind: "task_unblocked",
		Effects:   []EffectSpec{{Topic: "run.launch", KeyTemplate: "resume:{{run_id}}"}},
	},
	{
		From: TaskRunning, Trigger: TrCancel, To: TaskCancelled,
		EventKind: "task_cancelled",
		Effects:   []EffectSpec{{Topic: "run.kill", KeyTemplate: "kill:{{run_id}}"}},
	},
	{
		From: TaskBlockedOnHuman, Trigger: TrCancel, To: TaskCancelled,
		EventKind: "task_cancelled",
		Effects:   []EffectSpec{{Topic: "run.kill", KeyTemplate: "kill:{{run_id}}"}},
	},
	{
		From: TaskRunning, Trigger: TrPark, To: TaskParked,
		EventKind: "task_parked",
	},
	{
		From: TaskBlockedOnHuman, Trigger: TrPark, To: TaskParked,
		EventKind: "task_parked",
	},
	{
		From: TaskReview, Trigger: TrApproved, To: TaskDone,
		EventKind: "task_done",
	},
	{
		From: TaskReview, Trigger: TrCancel, To: TaskCancelled,
		EventKind: "task_cancelled",
	},
}

// TaskTable returns the task state machine's rows, for the golden-file dump
// and for any tooling (e.g. a future docs generator) that wants to render
// it. Callers must not mutate the result.
func TaskTable() []TaskTransition {
	return taskTable
}

// NextTask is the pure task-lifecycle transition function: (state, trigger,
// context) -> (state, event, effects). No IO, no clock, no randomness — same
// inputs, same output, forever. Returns ErrIllegalTransition, never a silent
// no-op, when (from, tr) has no matching row.
func NextTask(from TaskState, tr Trigger, tc TransitionContext) (Outcome, error) {
	for _, row := range taskTable {
		if row.From != from || row.Trigger != tr {
			continue
		}

		effects := make([]PendingEffect, 0, len(row.Effects))
		for _, spec := range row.Effects {
			effects = append(effects, PendingEffect{Topic: spec.Topic, Key: spec.RenderKey(tc)})
		}

		return Outcome{
			To: row.To,
			Event: PendingEvent{
				Kind:    row.EventKind,
				Actor:   ActorControlPlane,
				Payload: eventPayload(tc),
			},
			Effects: effects,
		}, nil
	}

	return Outcome{}, fmt.Errorf("%w: task state=%s trigger=%s", ErrIllegalTransition, from, tr)
}

// RunTransition is one row of the run state machine — deliberately much
// smaller than the task table: a run's lifecycle is "start, then exit one
// way," everything richer (retry, requeue, park) is task-level policy
// layered on top by internal/reconcile and the task table above.
type RunTransition struct {
	From      RunState
	Trigger   Trigger
	To        RunState
	EventKind string
}

var runTable = []RunTransition{
	{From: RunPending, Trigger: TrRunStarted, To: RunStarting, EventKind: "run_starting"},
	{From: RunStarting, Trigger: TrRunStarted, To: RunRunning, EventKind: "run_running"},
	{From: RunRunning, Trigger: TrRunExitedOK, To: RunExited, EventKind: "run_exited"},
	{From: RunRunning, Trigger: TrRunFailed, To: RunFailed, EventKind: "run_failed"},
	{From: RunPending, Trigger: TrCancel, To: RunCancelled, EventKind: "run_cancelled"},
	{From: RunStarting, Trigger: TrCancel, To: RunCancelled, EventKind: "run_cancelled"},
	{From: RunRunning, Trigger: TrCancel, To: RunCancelled, EventKind: "run_cancelled"},
}

// RunTable returns the run state machine's rows.
func RunTable() []RunTransition {
	return runTable
}

// RunOutcome mirrors Outcome for the (smaller) run state machine — a run
// transition never itself schedules an outbox effect; the task transition
// that observes the run's outcome (TrRunExitedErrRetryable/Final on the task
// table) does that.
type RunOutcome struct {
	To    RunState
	Event PendingEvent
}

// NextRun is NextTask's counterpart for run.state.
func NextRun(from RunState, tr Trigger, tc TransitionContext) (RunOutcome, error) {
	for _, row := range runTable {
		if row.From != from || row.Trigger != tr {
			continue
		}

		return RunOutcome{
			To:    row.To,
			Event: PendingEvent{Kind: row.EventKind, Actor: ActorControlPlane, Payload: eventPayload(tc)},
		}, nil
	}

	return RunOutcome{}, fmt.Errorf("%w: run state=%s trigger=%s", ErrIllegalTransition, from, tr)
}
