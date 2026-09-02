package domain

// TaskState is one of the task lifecycle states from development-plan.md §3's
// state machine diagram. The literal string values are load-bearing: they
// are mirrored verbatim by deploy/migrations/0002_control_plane.up.sql's
// task_state_ck CHECK constraint, so the two must never drift apart.
type TaskState string

const (
	TaskCreated        TaskState = "CREATED"
	TaskQueued         TaskState = "QUEUED"
	TaskRunning        TaskState = "RUNNING"
	TaskReview         TaskState = "REVIEW"
	TaskDone           TaskState = "DONE"
	TaskBlockedOnHuman TaskState = "BLOCKED_ON_HUMAN"
	TaskFailed         TaskState = "FAILED"
	TaskCancelled      TaskState = "CANCELLED"
	TaskParked         TaskState = "PARKED"
)

// AllTaskStates lists every TaskState, in the order the state machine table
// is easiest to read against — used by the golden-file dump and by the
// transition-grid test to enumerate every (state, trigger) cell.
func AllTaskStates() []TaskState {
	return []TaskState{
		TaskCreated, TaskQueued, TaskRunning, TaskReview, TaskDone,
		TaskBlockedOnHuman, TaskFailed, TaskCancelled, TaskParked,
	}
}

// RunState is one of the run lifecycle states. Unlike TaskState, §3 names
// the run.state column but never enumerates its values — this set is an
// internal/domain design decision, mirrored by run_state_ck in
// 0002_control_plane.up.sql (see that migration's sourcing comment).
type RunState string

const (
	RunPending   RunState = "PENDING"
	RunStarting  RunState = "STARTING"
	RunRunning   RunState = "RUNNING"
	RunExited    RunState = "EXITED"
	RunFailed    RunState = "FAILED"
	RunCancelled RunState = "CANCELLED"
)

// AllRunStates lists every RunState.
func AllRunStates() []RunState {
	return []RunState{RunPending, RunStarting, RunRunning, RunExited, RunFailed, RunCancelled}
}

// Trigger names the event that drives a state transition. Triggers are the
// domain's own vocabulary, not dsh's or Postgres's — internal/api and
// internal/supervisor translate their inputs (an HTTP request, a container
// exit, a heartbeat timeout) into a Trigger before calling NextTask/NextRun.
type Trigger string

const (
	TrIngested    Trigger = "ingested"      // tasks:ingest created this task
	TrStart       Trigger = "start"         // POST /v1/tasks/{id}/start
	TrRunStarted  Trigger = "run_started"   // supervisor confirmed the container is up
	TrRunExitedOK Trigger = "run_exited_ok" // run finished, task's work is ready for review
	// TrRunFailed drives RunState (run.state has no retry concept of its
	// own — a run either exited ok or in error, full stop; NextRun below
	// never branches on attempt count).
	TrRunFailed Trigger = "run_failed"
	// TrRunExitedErrRetryable and TrRunExitedErrFinal are deliberately
	// distinct triggers, not one "run_exited_err" the domain layer branches
	// on internally: NextTask is pure and has no attempt counter to
	// consult, so the caller (internal/reconcile, P8) decides retry-vs-give-up
	// from run.attempt against a configured max BEFORE calling NextTask, and
	// picks the trigger that encodes that decision.
	TrRunExitedErrRetryable Trigger = "run_exited_err_retryable" // requeues the task for another attempt
	TrRunExitedErrFinal     Trigger = "run_exited_err_final"     // attempts exhausted; task fails
	TrAsked                 Trigger = "asked"                    // af-ask-human raised a question (M3; envelope only in M2)
	TrAnswered              Trigger = "answered"                 // a human answered (M3), or the orchestrator answered a worker (M5)
	TrCancel                Trigger = "cancel"                   // POST /v1/tasks/{id}/cancel
	TrPark                  Trigger = "park"                     // the M3 timeout ladder's terminal step
	TrApproved              Trigger = "approved"                 // POST /v1/approvals (M4) approves the REVIEW artifact
	// TrAskedOrchestrator is TrAsked's counterpart for D7's ask_orchestrator
	// (M5): a worker blocking on its orchestrator, not a human. Kept as its
	// own trigger rather than reusing TrAsked with a flag because TrAsked's
	// row unconditionally schedules the zulip.question effect — an
	// orchestrator-bound question must never reach Zulip.
	TrAskedOrchestrator Trigger = "asked_orchestrator"
)
