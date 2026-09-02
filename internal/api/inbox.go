package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/domain"
)

// inboxMessage is one envelope af-control's inbox long-poll loop
// (runner/packages/af-control, P6) can receive. "cancel" is the only kind
// M2 actually produces (POST /v1/tasks/{id}/cancel while RUNNING/
// BLOCKED_ON_HUMAN enqueues run.kill and moves run.state toward
// CANCELLED). "answer" carries the shape M3's af-ask-human round-trip
// declared, but a human's answer is actually delivered as an env var at
// resume launch (AF_RESUME_ANSWER — internal/supervisor's daemon.spec()),
// never through this poll, so that kind stays unreachable here too. M5
// adds the two kinds that ARE delivered this way: "worker_question" (D7's
// ask_orchestrator, for the orchestrator's own check_workers tool) and
// "worker_report" (report_to_orchestrator) — both read off run_inbox.
type inboxMessage struct {
	Kind     string  `json:"kind"`
	Question *string `json:"question_id,omitempty"`
	Answer   *string `json:"answer,omitempty"`
	// Payload carries run_inbox's own stored JSON verbatim for
	// worker_question/worker_report — it already includes from_run_id, so
	// this envelope doesn't duplicate that field at the top level.
	Payload json.RawMessage `json:"payload,omitempty"`
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
// now. It never blocks; the caller's loop supplies the waiting. Checked in
// order:
//
//  1. The run's own state having reached CANCELLED (§4's cancel delivery)
//     — checked first so a cancelled run is never handed more work instead
//     of the cancellation it's waiting for.
//  2. run_inbox (M5): a worker_question (D7's ask_orchestrator) or
//     worker_report (report_to_orchestrator) waiting for this run —
//     ClaimNextRunInbox atomically marks it delivered (FOR UPDATE SKIP
//     LOCKED), so a redelivered long-poll request never claims the same
//     row twice.
//
// The "answer" kind inboxMessage declares has no producer here: a human's
// answer is delivered as an env var at resume launch (AF_RESUME_ANSWER —
// internal/supervisor's daemon.spec()), never through this poll.
func (s *Server) pollInbox(ctx context.Context, runID uuid.UUID) (inboxMessage, bool) {
	run, err := s.Store.Q().GetRunByID(ctx, runID)
	if err == nil && run.State == string(domain.RunCancelled) {
		return inboxMessage{Kind: "cancel"}, true
	}

	item, err := s.Store.Q().ClaimNextRunInbox(ctx, pgtype.UUID{Bytes: runID, Valid: true})
	if err == nil {
		return inboxMessage{Kind: item.Kind, Payload: item.Payload}, true
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		s.Log.Error("api: claiming run_inbox failed", "run_id", runID, "error", err)
	}

	return inboxMessage{}, false
}
