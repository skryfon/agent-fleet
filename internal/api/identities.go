package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// getIdentityByZulip is GET /v1/identities/by-zulip/{zulip_user_id} —
// cmd/bridge's own identity-verification step (development-plan.md §6:
// "Verify the sender maps to a known identity ... Unmapped senders are
// ignored and logged"). The bridge is deliberately stateless and holds no
// database access of its own (development-plan.md §2's "bridge ...
// Stateless"), so this lookup crosses the same API boundary every other
// runner-adjacent decision does.
func (s *Server) getIdentityByZulip(w http.ResponseWriter, r *http.Request) {
	zulipUserID := r.PathValue("zulip_user_id")
	if zulipUserID == "" {
		writeError(w, http.StatusBadRequest, "zulip_user_id is required")

		return
	}

	identity, err := s.Store.Q().GetIdentityByZulipUserID(r.Context(), &zulipUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no identity mapped to this zulip_user_id")

			return
		}

		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, identity)
}
