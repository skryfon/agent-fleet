package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	db "agentfleet/internal/store/gen"

	"agentfleet/internal/domain/manifest"
)

type createProjectRequest struct {
	Slug        string   `json:"slug"`
	ManifestRef string   `json:"manifest_ref"`
	Repos       []string `json:"repos"`
	// Manifest is the raw .agentfleet/project.yaml body (M6,
	// development-plan.md §5/§7). Parsed and schema/cross-field validated
	// by manifest.Parse before anything is written — a bad manifest is
	// rejected at registration, not discovered at run start. Optional: an
	// empty Manifest registers a project that falls back to
	// api.Server.Manifest/BudgetCaps/FanoutCaps, same as one registered
	// before M6.
	Manifest string `json:"manifest"`
}

// manifestHash is sha256(rawManifest) — never the client-supplied value.
// Before M6, manifest_hash was whatever string the caller sent (any
// string, unvalidated); it is now the actual integrity claim its own name
// promises: a revised manifest is detectable by comparing this against a
// fresh hash of the stored bytes, the same "hash voids on revision"
// discipline approval.subject_sha256 already uses (.claude/CLAUDE.md).
func manifestHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))

	return hex.EncodeToString(sum[:])
}

// parseAndCompileManifest validates raw (empty is valid — the no-manifest
// fallback case) and returns its compiled JSON form ready for the
// project.manifest column. A non-empty raw that fails to parse returns the
// issues formatted as one newline-joined string, suitable for a 400 body.
func parseAndCompileManifest(raw string) ([]byte, []manifest.Issue) {
	if raw == "" {
		return []byte(`{}`), nil
	}

	m, issues := manifest.Parse([]byte(raw))
	if len(issues) > 0 {
		return nil, issues
	}

	compiled, err := json.Marshal(m)
	if err != nil {
		// Manifest is a plain struct of strings/slices/maps — Marshal
		// cannot fail on it; a failure here would be a manifest.go bug, not
		// a caller input problem.
		return nil, []manifest.Issue{{Message: "internal error encoding manifest: " + err.Error()}}
	}

	return compiled, nil
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	if req.Slug == "" || req.ManifestRef == "" {
		writeError(w, http.StatusBadRequest, "slug and manifest_ref are required")

		return
	}

	if req.Repos == nil {
		req.Repos = []string{}
	}

	compiled, issues := parseAndCompileManifest(req.Manifest)
	if len(issues) > 0 {
		writeError(w, http.StatusBadRequest, "invalid manifest: "+issuesString(issues))

		return
	}

	proj, err := s.Store.Q().CreateProject(r.Context(), db.CreateProjectParams{
		Slug: req.Slug, ManifestRef: req.ManifestRef, ManifestHash: manifestHash(req.Manifest),
		Repos: req.Repos, Status: "ACTIVE", Manifest: compiled,
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

type updateProjectManifestRequest struct {
	ManifestRef string `json:"manifest_ref"`
	Manifest    string `json:"manifest"`
}

// updateProjectManifest is PUT /v1/projects/{slug}/manifest (M6): a
// manifest revision is not a DELETE + re-register of the whole project —
// its feature/task/run history stays put. Same parse-hash-store path as
// createProject.
func (s *Server) updateProjectManifest(w http.ResponseWriter, r *http.Request) {
	var req updateProjectManifestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")

		return
	}

	if req.ManifestRef == "" {
		writeError(w, http.StatusBadRequest, "manifest_ref is required")

		return
	}

	compiled, issues := parseAndCompileManifest(req.Manifest)
	if len(issues) > 0 {
		writeError(w, http.StatusBadRequest, "invalid manifest: "+issuesString(issues))

		return
	}

	proj, err := s.Store.Q().UpdateProjectManifest(r.Context(), db.UpdateProjectManifestParams{
		Slug: r.PathValue("slug"), Manifest: compiled, ManifestRef: req.ManifestRef, ManifestHash: manifestHash(req.Manifest),
	})
	if err != nil {
		writeDBErr(w, s.Log, http.StatusNotFound, "project not found", err)

		return
	}

	writeJSON(w, http.StatusOK, proj)
}

func issuesString(issues []manifest.Issue) string {
	var b []byte

	for i, iss := range issues {
		if i > 0 {
			b = append(b, '\n')
		}

		b = append(b, iss.String()...)
	}

	return string(b)
}
