package api

import (
	"encoding/json"
	"net/http"

	db "agentfleet/internal/store/gen"
)

type createFeatureRequest struct {
	Slug       string  `json:"slug"`
	SpecRef    *string `json:"spec_ref"`
	ZulipTopic *string `json:"zulip_topic"`
}

func (s *Server) createFeature(w http.ResponseWriter, r *http.Request) {
	proj, err := s.Store.Q().GetProjectBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")

		return
	}

	var req createFeatureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	if req.Slug == "" {
		writeError(w, http.StatusBadRequest, "slug is required")

		return
	}

	feat, err := s.Store.Q().CreateFeature(r.Context(), db.CreateFeatureParams{
		ProjectID: proj.ID, Slug: req.Slug, SpecRef: req.SpecRef, ZulipTopic: req.ZulipTopic, State: "OPEN",
	})
	if err != nil {
		writeDBErr(w, s.Log, http.StatusConflict, "feature could not be created (slug likely already exists on this project)", err)

		return
	}

	writeJSON(w, http.StatusCreated, feat)
}

func (s *Server) listFeatures(w http.ResponseWriter, r *http.Request) {
	proj, err := s.Store.Q().GetProjectBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")

		return
	}

	features, err := s.Store.Q().ListFeaturesByProject(r.Context(), proj.ID)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, features)
}
