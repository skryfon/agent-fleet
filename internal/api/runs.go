package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"agentfleet/internal/domain"
	"agentfleet/internal/store"
)

type mirrorEventRequest struct {
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"at"`
}

type postRunEventsRequest struct {
	Events []mirrorEventRequest `json:"events"`
}

type postRunEventsResponse struct {
	Accepted     int   `json:"accepted"`
	Duplicates   int   `json:"duplicates"`
	HighWaterSeq int64 `json:"high_water_seq"`
}

// postRunEvents is af-control's batch mirror POST — docs/adr/0001's
// idempotent-mirror invariant lives in store.AppendMirror's ON CONFLICT DO
// NOTHING against event_dsh_seq_uk; this handler is a thin translation
// layer over it. highWaterSeq in the response is what af-control reseeds
// its own cursor from — the plan is explicit that value is "never guessed
// client-side."
func (s *Server) postRunEvents(w http.ResponseWriter, r *http.Request) {
	run, _ := runFromContext(r.Context())

	var req postRunEventsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	batch := store.MirrorBatch{RunID: run.ID, TaskID: run.TaskID, Events: make([]store.MirrorEvent, len(req.Events))}
	for i, ev := range req.Events {
		batch.Events[i] = store.MirrorEvent{Seq: ev.Seq, Kind: ev.Kind, Actor: ev.Actor, Payload: ev.Payload, At: ev.At}
	}

	result, err := s.Store.AppendMirror(r.Context(), s.Redact, batch)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	highWater, err := s.Store.MirrorHighWaterSeq(r.Context(), run.ID)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, postRunEventsResponse{
		Accepted: result.Accepted, Duplicates: result.Duplicates, HighWaterSeq: highWater,
	})
}

// checkpoint is both the run's heartbeat (internal/reconcile's stale-run
// sweep, P8, reads last_heartbeat_at back out) and, per the plan, where a
// run's own checkpoint blob would land — the request body's checkpoint
// field is accepted and currently discarded; wiring it into run.checkpoint
// is a small follow-up once a consumer of that column exists, not required
// for M2's own done-condition.
func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request) {
	run, _ := runFromContext(r.Context())

	if err := s.Store.Q().TouchRunHeartbeat(r.Context(), run.ID); err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// maxTaskAttempts caps automatic requeue-on-failure before a task gives up
// (TrRunExitedErrRetryable vs. TrRunExitedErrFinal). A fixed constant here,
// not yet a manifest/config knob — nothing in the codebase exposes one for
// this yet; a documented M2 placeholder, tightened when M4's budget/M8's
// config surface exists. Compared against task.attempt, not run.attempt —
// see 0002_control_plane.up.sql's sourcing comment on task.attempt for why.
const maxTaskAttempts = 3

type containerReportRequest struct {
	ContainerID string `json:"container_id"`
	// ExitCode's presence distinguishes the two report shapes this one
	// endpoint carries: nil means "container started" (persists
	// container_id, advances run.state one step via the domain table);
	// non-nil means "container exited" (decides retryable-vs-final from
	// run.attempt and drives both run.state and task.state atomically via
	// ApplyRunExit).
	ExitCode *int32 `json:"exit_code"`
}

// containerReport is internal/supervisor's callback (P5's cmd/supervisor is
// its real caller; internal/reconcile, P8, may also call it for a run it
// discovers has already exited). Kept intentionally thin: the concurrency
// semaphore, container watching, and orphan reaping that decide WHEN to
// call this are P5/P8 scope, not this handler's.
func (s *Server) containerReport(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run id")

		return
	}

	var req containerReportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	if req.ExitCode == nil {
		s.reportContainerStarted(w, r, runID, req.ContainerID)

		return
	}

	s.reportContainerExited(w, r, runID, *req.ExitCode)
}

func (s *Server) reportContainerStarted(w http.ResponseWriter, r *http.Request, runID uuid.UUID, containerID string) {
	// ContainerID rides in the SAME transaction as the state transition
	// (see RunTransitionRequest.ContainerID's doc comment) — a prior
	// version wrote this as a second, separate statement after commit,
	// which DB review caught as a real lost-update race against any other
	// concurrent writer of this run row.
	transition, err := s.Store.ApplyRunTransition(r.Context(), s.Redact, store.RunTransitionRequest{
		RunID: runID, Trigger: domain.TrRunStarted, Actor: "supervisor", ContainerID: &containerID,
	})
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, transition)
}

func (s *Server) reportContainerExited(w http.ResponseWriter, r *http.Request, runID uuid.UUID, exitCode int32) {
	run, err := s.Store.Q().GetRunByID(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusNotFound, "run not found")

		return
	}

	runTrigger, taskTrigger := domain.TrRunExitedOK, domain.TrRunExitedOK
	if exitCode != 0 {
		runTrigger = domain.TrRunFailed

		task, err := s.Store.Q().GetTaskByID(r.Context(), run.TaskID)
		if err != nil {
			writeError(w, http.StatusNotFound, "task not found")

			return
		}

		if task.Attempt < maxTaskAttempts {
			taskTrigger = domain.TrRunExitedErrRetryable
		} else {
			taskTrigger = domain.TrRunExitedErrFinal
		}
	}

	// ExitCode and the attempt increment ride in the SAME transaction as
	// both state transitions (run.state then task.state) — see
	// RunExitRequest.ExitCode/IncrementAttempt's doc comments for why a
	// separate post-commit write here was a real race and a
	// non-self-healing crash gap, per DB review.
	result, err := s.Store.ApplyRunExit(r.Context(), s.Redact, store.RunExitRequest{
		RunID: runID, RunTrigger: runTrigger, TaskTrigger: taskTrigger, Actor: "supervisor",
		ExitCode: &exitCode, IncrementAttempt: taskTrigger == domain.TrRunExitedErrRetryable,
	})
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listActiveRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.Q().ListActiveRuns(r.Context())
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, runs)
}
