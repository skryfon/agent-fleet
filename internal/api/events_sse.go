package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "agentfleet/internal/store/gen"
)

const sseBatchLimit = 200

// eventsSSE serves GET /v1/events?since=<RFC3339 timestamp>: an
// initial catch-up burst of every event at or after since (event_at_id_idx,
// the total read order §4/P4 names), then a live tail streamed as
// server-sent events, polling for new rows until the client disconnects.
// This is a straightforward polling SSE, not a Postgres LISTEN/NOTIFY feed
// — simplest thing that works for M2's own dashboards; a push-based feed is
// a later optimization, not a behavior change, if polling latency ever
// matters.
func (s *Server) eventsSSE(w http.ResponseWriter, r *http.Request) {
	since := time.Time{}

	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC3339")

			return
		}

		since = t
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	cursorAt, cursorID := since, int64(0)
	ticker := time.NewTicker(inboxPollEvery)

	defer ticker.Stop()

	for {
		events, err := s.Store.Q().ListEventsSince(ctx, db.ListEventsSinceParams{
			SinceAt: pgtype.Timestamptz{Time: cursorAt, Valid: true}, SinceID: cursorID, Limit: sseBatchLimit,
		})
		if err != nil {
			s.Log.Error("api: events SSE query failed", "error", err)

			return
		}

		for _, ev := range events {
			body, err := json.Marshal(ev)
			if err != nil {
				continue
			}

			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, body); err != nil {
				return
			}

			if ev.At.Valid {
				cursorAt, cursorID = ev.At.Time, ev.ID
			}
		}

		flusher.Flush()

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
