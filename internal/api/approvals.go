package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

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

// pendingApproval is one card in GET /v1/approvals/pending — the webapp's
// approval queue view (development-plan.md §7 M7). Artifact is nil when the
// task is in REVIEW but has not yet had a PR artifact recorded; the webapp
// renders that as "no artifact" with both decision buttons disabled, per
// approval.subject_sha256's mandatory-hash-binding invariant (§3) — there is
// nothing to approve against yet.
type pendingApproval struct {
	TaskID      string  `json:"task_id"`
	Title       string  `json:"title"`
	Intent      string  `json:"intent"`
	Lane        string  `json:"lane"`
	Role        *string `json:"role,omitempty"`
	FeatureSlug string  `json:"feature_slug"`
	ZulipTopic  *string `json:"zulip_topic,omitempty"`
	ProjectSlug string  `json:"project_slug"`

	Artifact *pendingApprovalArtifact `json:"artifact"`
}

type pendingApprovalArtifact struct {
	Kind   string `json:"kind"`
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

// listPendingApprovals is GET /v1/approvals/pending. See
// store/queries/approval.sql's ListPendingApprovals doc comment for why the
// artifact lookup is a per-task follow-up query rather than a join.
func (s *Server) listPendingApprovals(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Store.Q().ListPendingApprovals(r.Context())
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	out := make([]pendingApproval, len(tasks))

	for i, t := range tasks {
		out[i] = pendingApproval{
			TaskID: t.TaskID.String(), Title: t.Title, Intent: t.Intent, Lane: t.Lane,
			Role: t.Role, FeatureSlug: t.FeatureSlug, ZulipTopic: t.ZulipTopic, ProjectSlug: t.ProjectSlug,
		}

		artifact, err := s.Store.Q().GetLatestArtifactByTask(r.Context(), t.TaskID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}

			writeTransitionErr(w, s.Log, err)

			return
		}

		out[i].Artifact = &pendingApprovalArtifact{Kind: artifact.Kind, URI: artifact.Uri, SHA256: artifact.Sha256}
	}

	writeJSON(w, http.StatusOK, out)
}
