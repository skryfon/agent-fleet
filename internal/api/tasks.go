package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"

	"agentfleet/internal/domain"
	"agentfleet/internal/domain/tasksmd"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
)

// ingestIssue mirrors tasksmd.Issue for the 422 response body — a distinct
// type (rather than exposing tasksmd.Issue's Go-only String() method
// directly) keeps the wire contract explicit.
type ingestIssue struct {
	Line    int    `json:"line,omitempty"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// ingestTasks implements tasks:ingest (development-plan.md §4 / §7 M2): the
// request body is the raw tasks.md file. No partial ingestion ever for a
// VALIDATION failure — any issue tasksmd.Parse finds means nothing is
// written (422, full issue list). Re-POSTing byte-identical content is a
// no-op 200 keyed off feature.tasks_md_sha256; changing tasks.md while any
// of its existing tasks is running (state not CREATED/QUEUED) is a 409,
// since rewriting the spec under a running agent is not allowed.
//
// Once validation passes, the per-task writes below are NOT one atomic
// transaction (each Upsert/Insert + its TrIngested transition is its own
// round trip) — flagged in code review. A mid-loop failure (a DB hiccup,
// not a validation issue) can leave a partial task set committed while
// feature.tasks_md_sha256 stays unset (only written after the whole loop
// succeeds), so a retry re-parses and re-attempts. This happens to be
// safe to retry: UpsertTaskByExternalRef is keyed by (feature_id,
// external_ref) and passes the row's OWN prior state through rather than
// assuming CREATED, and TrIngested is only fired for a row still CREATED —
// so a retried task that already advanced past CREATED on a prior partial
// attempt is left alone rather than double-transitioned. Accepted as-is for
// M2; a real transactional multi-task ingest (needing one WithTx spanning
// several ApplyTaskTransition-shaped writes) is a larger refactor than this
// gap currently justifies.
func (s *Server) ingestTasks(w http.ResponseWriter, r *http.Request) {
	featureID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid feature id")

		return
	}

	feat, err := s.Store.Q().GetFeatureByID(r.Context(), featureID)
	if err != nil {
		writeError(w, http.StatusNotFound, "feature not found")

		return
	}

	src, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())

		return
	}

	sum := sha256.Sum256(src)
	shaHex := hex.EncodeToString(sum[:])

	if feat.TasksMdSha256 != nil && *feat.TasksMdSha256 == shaHex {
		tasks, err := s.Store.Q().ListTasksByFeature(r.Context(), featureID)
		if err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}

		writeJSON(w, http.StatusOK, tasks)

		return
	}

	doc, issues := tasksmd.Parse(src)
	if len(issues) > 0 {
		out := make([]ingestIssue, len(issues))
		for i, iss := range issues {
			out[i] = ingestIssue{Line: iss.Line, Path: iss.Path, Message: iss.Message}
		}

		writeJSON(w, http.StatusUnprocessableEntity, struct {
			Issues []ingestIssue `json:"issues"`
		}{out})

		return
	}

	ordered, err := topoOrder(doc)
	if err != nil {
		// tasksmd.Parse already rejects cycles/unresolvable depends_on, so
		// this can only fire on an internal/api bug, not user input.
		writeError(w, http.StatusInternalServerError, err.Error())

		return
	}

	existing, err := s.Store.Q().ListTasksByFeature(r.Context(), featureID)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	byRef := make(map[string]db.Task, len(existing))
	for _, t := range existing {
		if t.ExternalRef != nil {
			byRef[*t.ExternalRef] = t
		}
	}

	for _, t := range ordered {
		if prev, ok := byRef[t.ExternalRef]; ok && prev.State != string(domain.TaskCreated) && prev.State != string(domain.TaskQueued) {
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"task %q is %s — tasks.md cannot be re-ingested while any of its tasks is running", t.ExternalRef, prev.State,
			))

			return
		}
	}

	ids := make(map[string]uuid.UUID, len(ordered))
	created := make([]db.Task, 0, len(ordered))

	for _, t := range ordered {
		acJSON, err := json.Marshal(t.AcceptanceCriteria)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())

			return
		}

		refsJSON, err := json.Marshal(t.SpecRefs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())

			return
		}

		deps := make([]uuid.UUID, 0, len(t.DependsOn))
		for _, dep := range t.DependsOn {
			deps = append(deps, ids[dep]) // ordered topologically, so dep is always already resolved
		}

		touches := t.Touches
		if touches == nil {
			touches = []string{}
		}

		externalRef := t.ExternalRef

		var row db.Task
		if prev, ok := byRef[t.ExternalRef]; ok {
			row, err = s.Store.Q().UpsertTaskByExternalRef(r.Context(), db.UpsertTaskByExternalRefParams{
				FeatureID: featureID, ExternalRef: &externalRef, Lane: t.Lane, Title: t.Title, Intent: t.Intent,
				AcceptanceCriteria: acJSON, Touches: touches, DependsOn: deps, SpecRefs: refsJSON, State: prev.State,
			})
		} else {
			row, err = s.Store.Q().InsertTask(r.Context(), db.InsertTaskParams{
				FeatureID: featureID, ExternalRef: &externalRef, Lane: t.Lane, Title: t.Title, Intent: t.Intent,
				AcceptanceCriteria: acJSON, Touches: touches, DependsOn: deps, SpecRefs: refsJSON, State: string(domain.TaskCreated),
			})
		}

		if err != nil {
			writeDBErr(w, s.Log, http.StatusInternalServerError, "writing task "+t.ExternalRef, err)

			return
		}

		ids[t.ExternalRef] = row.ID

		if row.State == string(domain.TaskCreated) {
			result, err := s.Store.ApplyTaskTransition(r.Context(), s.Redact, store.TransitionRequest{
				TaskID: row.ID, Trigger: domain.TrIngested, Actor: "tasks:ingest",
			})
			if err != nil {
				writeTransitionErr(w, s.Log, err)

				return
			}

			// The response must reflect the row's state AFTER TrIngested,
			// not the CREATED snapshot InsertTask returned — a caller that
			// re-lists tasks would otherwise see QUEUED while this
			// response's own body still claimed CREATED.
			row.State = string(result.To)
		}

		created = append(created, row)
	}

	if err := s.Store.Q().SetFeatureTasksMdSHA256(r.Context(), db.SetFeatureTasksMdSHA256Params{
		ID: featureID, TasksMdSha256: &shaHex,
	}); err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, created)
}

// topoOrder returns doc's tasks ordered so every task's depends_on entries
// come before it — tasksmd.Parse has already rejected cycles and
// unresolvable references, so a plain post-order DFS always succeeds.
// Needed because InsertTask/UpsertTaskByExternalRef take resolved task
// uuids for depends_on, but tasks.md itself only carries external_refs, and
// a later task may depend on an earlier one not yet assigned a uuid.
func topoOrder(doc *tasksmd.Doc) ([]tasksmd.Task, error) {
	byRef := make(map[string]tasksmd.Task, len(doc.Tasks))
	for _, t := range doc.Tasks {
		byRef[t.ExternalRef] = t
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)

	state := make(map[string]int, len(doc.Tasks))
	out := make([]tasksmd.Task, 0, len(doc.Tasks))

	var visit func(ref string) error

	visit = func(ref string) error {
		switch state[ref] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("topoOrder: unexpected cycle at %q (tasksmd.Parse should have caught this)", ref)
		}

		state[ref] = visiting

		for _, dep := range byRef[ref].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}

		state[ref] = done
		out = append(out, byRef[ref])

		return nil
	}

	for _, t := range doc.Tasks {
		if err := visit(t.ExternalRef); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (s *Server) listTasksByFeature(w http.ResponseWriter, r *http.Request) {
	featureID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid feature id")

		return
	}

	tasks, err := s.Store.Q().ListTasksByFeature(r.Context(), featureID)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) listTasksByState(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeError(w, http.StatusBadRequest, "?state= is required")

		return
	}

	tasks, err := s.Store.Q().ListTasksByState(r.Context(), state)
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) startTask(w http.ResponseWriter, r *http.Request) {
	s.applyTaskTrigger(w, r, domain.TrStart, "api:start")
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	s.applyTaskTrigger(w, r, domain.TrCancel, "api:cancel")
}

// applyTaskTrigger is startTask/cancelTask's shared body: parse the path
// id, apply the trigger, and translate the outcome to an HTTP response.
// Neither handler creates a Run row itself — the run.launch effect
// TrStart's transition schedules is handled by internal/supervisor.RunLaunch
// (P5), registered on cmd/control-plane's outbox relay.
func (s *Server) applyTaskTrigger(w http.ResponseWriter, r *http.Request, tr domain.Trigger, actor string) {
	taskID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid task id")

		return
	}

	var dedupeKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		dedupeKey = &k
	}

	result, err := s.Store.ApplyTaskTransition(r.Context(), s.Redact, store.TransitionRequest{
		TaskID: taskID, Trigger: tr, Actor: actor, DedupeKey: dedupeKey,
	})
	if err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	writeJSON(w, http.StatusOK, result)
}
