package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/store"
)

// answerQuestionRequest is POST /v1/questions/{id}/answer's body.
// answered_by is a free-form identity label (a Zulip display name / github
// login the caller has already resolved) — see this handler's doc comment
// for why identity VERIFICATION itself is not this endpoint's job.
type answerQuestionRequest struct {
	Answer     string `json:"answer"`
	AnsweredBy string `json:"answered_by"`
}

// answerQuestion is POST /v1/questions/{id}/answer (development-plan.md §4),
// replacing M2's documented 501. It applies TrAnswered via
// Store.ApplyAnswer, which returns the task to RUNNING and enqueues the
// run.launch/resume effect that resurrects the container.
//
// Identity verification — "verify the sender maps to a known identity ...
// unmapped senders are ignored and logged" (development-plan.md §6) — lives
// in cmd/bridge (M3 Phase 3), which resolves a Zulip sender via
// GetIdentityByZulipUserID BEFORE calling this endpoint. This handler trusts
// its authenticated caller (authAdmin today; the bridge holds ADMIN_TOKEN —
// see internal/api/api.go's route comment) and only enforces "the question
// is still OPEN," never who is answering.
func (s *Server) answerQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid question id")

		return
	}

	var req answerQuestionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	if req.Answer == "" {
		writeError(w, http.StatusBadRequest, "answer must not be empty")

		return
	}

	var dedupeKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		dedupeKey = &k
	}

	result, err := s.Store.ApplyAnswer(r.Context(), s.Redact, store.AnswerRequest{
		QuestionID: questionID,
		Answer:     req.Answer,
		AnsweredBy: req.AnsweredBy,
		Actor:      "api:answer_question",
		DedupeKey:  dedupeKey,
	})
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, answerQuestionResponse{
		QuestionID: result.Question.ID.String(),
		TaskState:  string(result.Task.To),
	})
}

type answerQuestionResponse struct {
	QuestionID string `json:"question_id"`
	TaskState  string `json:"task_state"`
}

// listQuestionsByZulipTopic is GET /v1/questions?zulip_topic=<topic> —
// cmd/bridge's other lookup: given the topic a reply landed in, find the
// one question question_one_open_per_feature_uk (0003_questions.up.sql)
// guarantees is the only OPEN one for that feature. 404 when the topic
// doesn't resolve to a feature, or the feature has no open question — both
// are "nothing for the bridge to answer," not a server error.
func (s *Server) listQuestionsByZulipTopic(w http.ResponseWriter, r *http.Request) {
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

	questions, err := s.Store.Q().ListOpenQuestionsByFeature(r.Context(), pgtype.UUID{Bytes: feature.ID, Valid: true})
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	if len(questions) == 0 {
		writeError(w, http.StatusNotFound, "no open question for this topic")

		return
	}

	writeJSON(w, http.StatusOK, questions[0])
}
