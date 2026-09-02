package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"agentfleet/internal/store"
)

// approvalRequest is POST /v1/approvals' body (development-plan.md §4:
// "{subject_ref, sha256, decision}"; subject_kind added here since the
// route also gates spec/plan approvals, not only 'pr' — see approval_
// subject_kind_ck).
type approvalRequest struct {
	SubjectKind string  `json:"subject_kind"`
	SubjectRef  string  `json:"subject_ref"`
	SHA256      string  `json:"sha256"`
	Decision    string  `json:"decision"`
	Note        *string `json:"note,omitempty"`
}

// createApproval is POST /v1/approvals — the hash-bound human gate on
// REVIEW->DONE (development-plan.md §3: "a revised artifact voids its
// approval"). A sha256 that doesn't match the artifact's CURRENT sha256 is
// a 409, never a silent approve of stale content.
func (s *Server) createApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request body: "+err.Error())

		return
	}

	var dedupeKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		dedupeKey = &k
	}

	result, err := s.Store.ApplyApproval(r.Context(), s.Redact, store.ApprovalRequest{
		SubjectKind: req.SubjectKind, SubjectRef: req.SubjectRef, Sha256: req.SHA256,
		Decision: req.Decision, Actor: "api:approve", Note: req.Note, DedupeKey: dedupeKey,
	})
	if err != nil {
		if errors.Is(err, store.ErrApprovalArtifactNotFound) {
			writeError(w, http.StatusNotFound, err.Error())

			return
		}

		if errors.Is(err, store.ErrApprovalSHAMismatch) {
			writeError(w, http.StatusConflict, err.Error())

			return
		}

		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}
