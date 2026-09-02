package api

import (
	"encoding/json"
	"net/http"

	"agentfleet/internal/domain"
	"agentfleet/internal/store"
)

type pauseRequest struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
	// Kill, when true, additionally cancels every RUNNING/BLOCKED_ON_HUMAN
	// task in scope (development-plan.md §4: "global + per-project kill
	// switch") — without it, pause only blocks NEW launches
	// (startTask/RunLaunch, both fail-closed on s.Store.CheckPause).
	Kill bool `json:"kill,omitempty"`
}

// pauseAdmin is POST /v1/admin/pause. Only "global" is actually enforced
// today — see internal/store.CheckPause's doc comment — but the row is
// recorded for any scope string a caller supplies, so a future per-project
// enforcement pass needs no schema change.
func (s *Server) pauseAdmin(w http.ResponseWriter, r *http.Request) {
	var req pauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request body: "+err.Error())

		return
	}

	if req.Scope == "" {
		writeError(w, http.StatusBadRequest, "scope is required")

		return
	}

	if _, err := s.Store.SetPause(r.Context(), s.Redact, req.Scope, "api:admin", req.Reason); err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	if req.Kill {
		s.killActiveTasks(r, req.Scope)
	}

	w.WriteHeader(http.StatusNoContent)
}

// killActiveTasks cancels every RUNNING/BLOCKED_ON_HUMAN task — the pause
// row itself is scope-general, but M4 doesn't yet resolve "which tasks
// belong to project X," so a kill always sweeps every active task
// regardless of the scope string passed (ponytail: correct for scope
// "global", overbroad for a "project:<id>" scope until that resolution
// exists — acceptable since the kill switch's whole point is "stop
// everything now," not surgical per-project targeting).
func (s *Server) killActiveTasks(r *http.Request, scope string) {
	for _, state := range []string{string(domain.TaskRunning), string(domain.TaskBlockedOnHuman)} {
		tasks, err := s.Store.Q().ListTasksByState(r.Context(), state)
		if err != nil {
			s.Log.Error("api: listing tasks to kill", "state", state, "error", err)

			continue
		}

		for _, task := range tasks {
			if _, err := s.Store.ApplyTaskTransition(r.Context(), s.Redact, store.TransitionRequest{
				TaskID: task.ID, Trigger: domain.TrCancel, Actor: "admin:pause:" + scope,
			}); err != nil {
				s.Log.Error("api: killing task on pause failed", "task_id", task.ID, "error", err)
			}
		}
	}
}

// resumeAdmin is DELETE /v1/admin/pause?scope=... — lifts the kill switch.
// Does not itself re-launch anything: a task the kill swept is CANCELLED,
// terminal, and needs a human to re-queue it, same as any other cancel.
func (s *Server) resumeAdmin(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		writeError(w, http.StatusBadRequest, "?scope= is required")

		return
	}

	if err := s.Store.ClearPause(r.Context(), s.Redact, scope, "api:admin"); err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}
