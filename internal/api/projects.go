package api

import (
	"encoding/json"
	"net/http"

	db "agentfleet/internal/store/gen"
)

type createProjectRequest struct {
	Slug         string   `json:"slug"`
	ManifestRef  string   `json:"manifest_ref"`
	ManifestHash string   `json:"manifest_hash"`
	Repos        []string `json:"repos"`
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	if req.Slug == "" || req.ManifestRef == "" || req.ManifestHash == "" {
		writeError(w, http.StatusBadRequest, "slug, manifest_ref, and manifest_hash are required")

		return
	}

	if req.Repos == nil {
		req.Repos = []string{}
	}

	proj, err := s.Store.Q().CreateProject(r.Context(), db.CreateProjectParams{
		Slug: req.Slug, ManifestRef: req.ManifestRef, ManifestHash: req.ManifestHash,
		Repos: req.Repos, Status: "ACTIVE",
	})
	if err != nil {
		writeDBErr(w, s.Log, http.StatusConflict, "project could not be created (slug likely already exists)", err)

		return
	}

	writeJSON(w, http.StatusCreated, proj)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.Q().ListProjects(r.Context())
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	proj, err := s.Store.Q().GetProjectBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")

		return
	}

	writeJSON(w, http.StatusOK, proj)
}
