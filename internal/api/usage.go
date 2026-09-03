package api

import (
	"encoding/json"
	"net/http"

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

	_, budgetCaps, _, err := s.resolveManifest(r.Context(), run)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	result, err := s.Store.RecordUsage(r.Context(), run.ID, task.FeatureID, store.UsageDelta{
		TokensIn: req.TokensIn, TokensOut: req.TokensOut, CostUSD: req.CostUSD, Minutes: req.Minutes,
	}, budgetCaps)
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

	actor := "budget:" + result.Breach.Kind

	// M5: a run-scope breach only cancels its own subtree (the reporting
	// task and anything it spawned) — the same "cap the runaway, not the
	// whole feature" instinct as fanout.Check's per-run children cap. A
	// FEATURE-scope breach means the aggregate is over budget regardless of
	// which task tipped it, so every active task in the feature is
	// cancelled, each as its own subtree root — otherwise a runaway
	// worker's siblings would keep burning the feature's already-exhausted
	// budget while this one task's own cancel is in flight.
	if result.BreachScope == "feature" {
		tasks, err := s.Store.Q().ListActiveTasksByFeature(r.Context(), task.FeatureID)
		if err != nil {
			s.Log.Error("api: listing active tasks for feature budget breach failed", "feature_id", task.FeatureID, "error", err)
		}

		for _, t := range tasks {
			if _, err := s.Store.CancelSubtree(r.Context(), s.Redact, t.ID, actor); err != nil {
				s.Log.Warn("api: cancelling task on feature budget breach failed", "task_id", t.ID, "error", err)
			}
		}
	} else if _, err := s.Store.CancelSubtree(r.Context(), s.Redact, task.ID, actor); err != nil {
		// The task may already be non-cancellable (e.g. a prior violation
		// already cancelled it) — an illegal transition here is not this
		// request's own failure to report, so log and still tell the caller
		// it breached.
		s.Log.Warn("api: cancelling task on run budget breach failed", "task_id", task.ID, "error", err)
	}

	writeJSON(w, http.StatusOK, usageResponse{Breached: true, Kind: result.Breach.Kind})
}
