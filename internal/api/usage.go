package api

import (
	"encoding/json"
	"net/http"

	"agentfleet/internal/domain"
	"agentfleet/internal/store"
)

// usageRequest is af-budget's report (runner/packages/af-budget) — a delta
// since its last report, read off ctx.tokenMeter and wall-clock minutes,
// never a running total (development-plan.md §4 M4, §6).
type usageRequest struct {
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	Minutes   int     `json:"minutes"`
}

type usageResponse struct {
	Breached bool   `json:"breached"`
	Kind     string `json:"kind,omitempty"`
}

// recordUsage is POST /v1/runs/{id}/usage. On breach it applies TrCancel
// itself (rather than making af-budget's own agent.cancel() the only stop)
// — two independent stops for the same breach, deliberately: the run may
// not notice or may be compromised past listening to its own hook.
func (s *Server) recordUsage(w http.ResponseWriter, r *http.Request) {
	run, _ := runFromContext(r.Context())

	var req usageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request body: "+err.Error())

		return
	}

	task, err := s.Store.Q().GetTaskByID(r.Context(), run.TaskID)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	result, err := s.Store.RecordUsage(r.Context(), run.ID, task.FeatureID, store.UsageDelta{
		TokensIn: req.TokensIn, TokensOut: req.TokensOut, CostUSD: req.CostUSD, Minutes: req.Minutes,
	}, s.BudgetCaps)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	if result.Breach == nil {
		writeJSON(w, http.StatusOK, usageResponse{})

		return
	}

	payload := map[string]any{"scope": result.BreachScope, "kind": result.Breach.Kind, "limit": result.Breach.Limit, "actual": result.Breach.Actual}
	if _, err := s.Store.RecordEvent(r.Context(), s.Redact, run.ID, "budget_breached", payload, nil); err != nil {
		s.Log.Error("api: recording budget_breached event failed", "error", err)
	}

	if _, err := s.Store.ApplyTaskTransition(r.Context(), s.Redact, store.TransitionRequest{
		TaskID: task.ID, RunID: &run.ID, Trigger: domain.TrCancel, Actor: "budget:" + result.Breach.Kind,
	}); err != nil {
		// The task may already be non-cancellable (e.g. a prior violation
		// already cancelled it) — an illegal transition here is not this
		// request's own failure to report, so log and still tell the caller
		// it breached.
		s.Log.Warn("api: cancelling task on budget breach failed", "task_id", task.ID, "error", err)
	}

	writeJSON(w, http.StatusOK, usageResponse{Breached: true, Kind: result.Breach.Kind})
}
