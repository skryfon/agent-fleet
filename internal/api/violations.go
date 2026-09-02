package api

import (
	"encoding/json"
	"net/http"
)

// violationRequest is af-policy's report (runner/packages/af-policy), for a
// local deny (`tools/pre-execute`) that internal/policy.Evaluate never saw
// — a mediated-tool deny already reaches Zulip via dispatchTool's own call
// to s.Store.RecordViolation (tools.go), so this route exists for the
// OTHER half of the two-layer story (development-plan.md §5's af-policy
// row).
type violationRequest struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

// reportViolation is POST /v1/runs/{id}/violations (development-plan.md §4
// M4: "the violation reaches Zulip within seconds"). source is always
// "runner" here; dispatchTool's own deny path (tools.go) calls
// Store.RecordViolation directly with source "control_plane" instead of
// looping back through HTTP.
func (s *Server) reportViolation(w http.ResponseWriter, r *http.Request) {
	run, _ := runFromContext(r.Context())

	var req violationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request body: "+err.Error())

		return
	}

	var dedupeKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		dedupeKey = &k
	}

	if _, err := s.Store.RecordViolation(r.Context(), s.Redact, run.ID, req.Tool, req.Reason, "runner", dedupeKey); err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}
