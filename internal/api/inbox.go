package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"agentfleet/internal/domain"
)

// inboxMessage is one envelope af-control's inbox long-poll loop
// (runner/packages/af-control, P6) can receive. "cancel" is the only kind
// M2 actually produces (POST /v1/tasks/{id}/cancel while RUNNING/
// BLOCKED_ON_HUMAN enqueues run.kill and moves run.state toward
// CANCELLED). "answer" carries the shape M3's af-ask-human round-trip will
// populate — the envelope exists now so af-control's inbox client doesn't
// need a schema change when the producing side lands; nothing in M2 ever
// asks a question, so this kind is currently unreachable in practice.
type inboxMessage struct {
	Kind     string  `json:"kind"`
	Question *string `json:"question_id,omitempty"`
	Answer   *string `json:"answer,omitempty"`
}

const (
	defaultInboxWait = 25 * time.Second
	maxInboxWait     = 60 * time.Second
	inboxPollEvery   = 500 * time.Millisecond
)

// inbox is the long-poll af-control's runner-side loop blocks on
// (development-plan.md §4). It returns as soon as a message is available,
// or 204 with no body once ?wait= elapses — af-control's own loop simply
// reconnects on either outcome, with backoff only on a network error, per
// the plan's description of that plugin's behavior.
func (s *Server) inbox(w http.ResponseWriter, r *http.Request) {
	run, _ := runFromContext(r.Context())

	wait := defaultInboxWait
	if v := r.URL.Query().Get("wait"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
	}

	if wait > maxInboxWait {
		wait = maxInboxWait
	}

	ctx := r.Context()
	deadline := time.Now().Add(wait)
	ticker := time.NewTicker(inboxPollEvery)

	defer ticker.Stop()

	for {
		if msg, ok := s.pollInbox(ctx, run.ID); ok {
			writeJSON(w, http.StatusOK, msg)

			return
		}

		if time.Now().After(deadline) {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pollInbox checks, once, whether there's a message worth delivering right
// now: the run's own state having reached CANCELLED (§4's cancel
// delivery). It never blocks; the caller's loop supplies the waiting.
//
// The "answer" kind inboxMessage declares has no producer yet:
// ListOpenQuestionsByRun only ever returns state='OPEN' rows (M3's
// af-ask-human, which would flip a question to ANSWERED via
// POST /v1/questions/{id}/answer, is not implemented — that route is a
// documented 501 in M2), so there is nothing for this function to poll for
// today. The envelope type exists so af-control's inbox client doesn't need
// a schema change when M3 lands; wiring the actual delivery is that
// milestone's work, not a stub worth writing against a query that can never
// answer it.
func (s *Server) pollInbox(ctx context.Context, runID uuid.UUID) (inboxMessage, bool) {
	run, err := s.Store.Q().GetRunByID(ctx, runID)
	if err == nil && run.State == string(domain.RunCancelled) {
		return inboxMessage{Kind: "cancel"}, true
	}

	return inboxMessage{}, false
}
