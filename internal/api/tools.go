package api

import (
	"io"
	"net/http"

	"agentfleet/internal/policy"
)

type dispatchToolResponse struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
	Rule   string `json:"rule,omitempty"`
}

// dispatchTool is the mediated-tool-dispatch endpoint
// (development-plan.md §4: "Mediated tools ... go through the API so the
// decision is recorded as an event"). The policy decision — allow or deny —
// is recorded as a control-plane event before this handler returns. M2
// stops at recording the decision: actually executing an allowed mediated
// tool (spawning a subagent, creating a PR) is M4/M5 scope layered on top
// of this same endpoint, not a new one.
func (s *Server) dispatchTool(w http.ResponseWriter, r *http.Request) {
	run, _ := runFromContext(r.Context())
	tool := r.PathValue("name")

	args, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())

		return
	}

	decision := policy.Evaluate(policy.Request{
		Role: run.Role, Tool: tool, Args: args, Manifest: s.Manifest,
	})

	kind := "tool_dispatch_allowed"
	if !decision.Allow {
		kind = "tool_dispatch_denied"
	}

	payload := map[string]any{"tool": tool, "rule": decision.Rule}
	if !decision.Allow {
		payload["reason"] = decision.Reason
	}

	// Flagged in DB review: unlike every transition-writing path,
	// RecordEvent originally had no way to dedupe a client-side retry (a
	// tool-dispatch client retrying a POST whose response was lost), which
	// would otherwise mint a second, distinct audit event via a fresh
	// IncrementRunEventSeq call instead of a no-op. Same Idempotency-Key
	// header convention applyTaskTrigger (tasks.go) already uses for task
	// lifecycle POSTs.
	var dedupeKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		dedupeKey = &k
	}

	if _, err := s.Store.RecordEvent(r.Context(), s.Redact, run.ID, kind, payload, dedupeKey); err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	status := http.StatusOK
	if !decision.Allow {
		status = http.StatusForbidden
	}

	writeJSON(w, status, dispatchToolResponse{Allow: decision.Allow, Reason: decision.Reason, Rule: decision.Rule})
}
