package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"agentfleet/internal/domain"
	db "agentfleet/internal/store/gen"
)

// reviewTaskOf returns the first task still in REVIEW — a feature has at
// most one in the ordinary flow (the M2 task table has no parallelism
// story yet), so "first" is "the" one.
func reviewTaskOf(tasks []db.Task) (db.Task, bool) {
	for _, t := range tasks {
		if t.State == string(domain.TaskReview) {
			return t, true
		}
	}

	return db.Task{}, false
}

type reviewByZulipTopicResponse struct {
	TaskID     string `json:"task_id"`
	SubjectRef string `json:"subject_ref"`
	SHA256     string `json:"sha256"`
}

// reviewByZulipTopic is GET /v1/tasks:review-by-zulip-topic?zulip_topic=...
// — cmd/bridge's own lookup (mirroring listQuestionsByZulipTopic's shape)
// for turning a Zulip reaction on a REVIEW notification into a
// POST /v1/approvals call: the topic resolves to a feature, the feature's
// most-recently-REVIEW task is the one being approved, and its latest
// artifact is what the approval's sha256 must match.
func (s *Server) reviewByZulipTopic(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("zulip_topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "zulip_topic is required")

		return
	}

	feature, err := s.Store.Q().GetFeatureByZulipTopic(r.Context(), &topic)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no feature for this zulip_topic")

			return
		}

		writeTransitionErr(w, s.Log, err)

		return
	}

	tasks, err := s.Store.Q().ListTasksByFeature(r.Context(), feature.ID)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	reviewTask, found := reviewTaskOf(tasks)
	if !found {
		writeError(w, http.StatusNotFound, "no task in REVIEW for this topic")

		return
	}

	artifact, err := s.Store.Q().GetLatestArtifactByTask(r.Context(), reviewTask.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no artifact recorded for this task")

			return
		}

		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, reviewByZulipTopicResponse{
		TaskID: reviewTask.ID.String(), SubjectRef: artifact.Uri, SHA256: artifact.Sha256,
	})
}
