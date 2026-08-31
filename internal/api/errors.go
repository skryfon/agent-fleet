package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"agentfleet/internal/domain"
)

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeTransitionErr renders domain.ErrIllegalTransition as 409 (the state
// machine's own "illegal transitions error, never silently no-op" contract
// surfaced to an HTTP caller), pgx.ErrNoRows (a GetTaskForUpdate/
// GetRunForUpdate miss inside a transition call) as 404, and anything else
// as a logged 500 with a generic client-facing message — a raw pgx/
// constraint error can name schema/column detail that has no business
// leaving the process, even to an admin-token holder (flagged in code
// review: writeDBErr below had this same gap for the plain CRUD handlers).
func writeTransitionErr(w http.ResponseWriter, log *slog.Logger, err error) {
	switch {
	case errors.Is(err, domain.ErrIllegalTransition):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not found")
	default:
		log.Error("api: internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// writeDBErr logs a raw database error server-side and returns a generic
// message to the client — used by the plain CRUD handlers (create
// project/feature) whose write can fail on a unique-constraint violation;
// the raw pgx error text (constraint name, column detail) is diagnostic
// information, not something to hand back over HTTP.
func writeDBErr(w http.ResponseWriter, log *slog.Logger, status int, context string, err error) {
	log.Error("api: "+context, "error", err)
	writeError(w, status, context)
}
