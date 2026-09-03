//go:build integration

package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"

	"agentfleet/internal/api"
	"agentfleet/internal/policy"
	"agentfleet/internal/redact"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
)

const adminToken = "test-admin-token" //nolint:gosec // test fixture, not a real credential

// newTestServer starts a real httptest server backed by a real Postgres
// (DATABASE_URL, the same convention every other *_integration_test.go in
// this repo uses — see internal/outbox/outbox_integration_test.go). Skips
// if DATABASE_URL isn't set, exactly like openTestStore elsewhere.
func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — see `make test-integration`")
	}

	st, err := store.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	t.Cleanup(st.Close)

	srv := &api.Server{
		Store:      st,
		Redact:     redact.New(nil, nil),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminToken: adminToken,
		Manifest: policy.Manifest{
			Roles: map[string]policy.Role{
				"implementer":  {MediatedTools: []string{"spawn_worker"}},
				"orchestrator": {MediatedTools: []string{"ask_human"}},
			},
		},
	}

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	return ts, st
}

func doJSON(t *testing.T, method, url, token string, body []byte) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}

	return resp, respBody
}

func TestAuthAdminRejectsMissingOrWrongToken(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, _ := doJSON(t, http.MethodGet, ts.URL+"/v1/projects", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/projects", "wrong-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodGet, ts.URL+"/v1/projects", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct token: status = %d, want 200", resp.StatusCode)
	}
}

// TestApprovalsAndPauseAreWired is a narrow smoke test that M4 actually
// replaced the M2 501 stubs — internal/store's own tests (once written)
// cover the real behavior; this only proves the HTTP surface is live.
func TestApprovalsAndPauseAreWired(t *testing.T) {
	ts, _ := newTestServer(t)

	// A nonexistent artifact is a 404, never the 501 these routes used to
	// return before M4.
	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/approvals", adminToken,
		[]byte(`{"subject_kind":"pr","subject_ref":"does-not-exist","sha256":"x","decision":"APPROVED"}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/v1/approvals with unknown subject_ref: status = %d, want 404", resp.StatusCode)
	}

	// Pause then resume the kill switch — both should succeed now that
	// they're real.
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/admin/pause", adminToken, []byte(`{"scope":"global","reason":"smoke test"}`))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /v1/admin/pause: status = %d, want 204", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodDelete, ts.URL+"/v1/admin/pause?scope=global", adminToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /v1/admin/pause: status = %d, want 204", resp.StatusCode)
	}
}

// TestProjectFeatureTaskLifecycle drives the full happy path this
// milestone's own done-condition names: project -> feature -> tasks:ingest
// -> task reaches QUEUED (TrIngested fires automatically) -> start ->
// RUNNING, all through the real HTTP surface, not internal/store directly.
func TestProjectFeatureTaskLifecycle(t *testing.T) {
	ts, _ := newTestServer(t)

	suffix := uuid.NewString()

	projBody, _ := json.Marshal(map[string]any{
		"slug": "proj-" + suffix, "manifest_ref": "main", "manifest_hash": "deadbeef",
	})

	resp, body := doJSON(t, http.MethodPost, ts.URL+"/v1/projects", adminToken, projBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project: status = %d, body = %s", resp.StatusCode, body)
	}

	var proj struct {
		ID   uuid.UUID `json:"id"`
		Slug string    `json:"slug"`
	}
	if err := json.Unmarshal(body, &proj); err != nil {
		t.Fatalf("decoding project: %v", err)
	}

	featBody, _ := json.Marshal(map[string]any{"slug": "feat-" + suffix})

	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/projects/"+proj.Slug+"/features", adminToken, featBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create feature: status = %d, body = %s", resp.StatusCode, body)
	}

	var feat struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(body, &feat); err != nil {
		t.Fatalf("decoding feature: %v", err)
	}

	tasksMD := []byte("```yaml agentfleet-tasks\n" +
		"version: v1\n" +
		"tasks:\n" +
		"  - external_ref: T1\n" +
		"    lane: direct\n" +
		"    title: Do the thing\n" +
		"    intent: because reasons\n" +
		"```\n")

	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/features/"+feat.ID.String()+"/tasks:ingest", adminToken, tasksMD)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest tasks: status = %d, body = %s", resp.StatusCode, body)
	}

	var tasks []struct {
		ID    uuid.UUID `json:"id"`
		State string    `json:"state"`
	}
	if err := json.Unmarshal(body, &tasks); err != nil {
		t.Fatalf("decoding ingested tasks: %v", err)
	}

	if len(tasks) != 1 || tasks[0].State != "QUEUED" {
		t.Fatalf("ingested tasks = %+v, want exactly one task in QUEUED (TrIngested should have fired)", tasks)
	}

	// Re-ingesting byte-identical content must be a no-op 200, per
	// tasks:ingest's own contract.
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/features/"+feat.ID.String()+"/tasks:ingest", adminToken, tasksMD)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-ingest identical tasks.md: status = %d, body = %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/tasks/"+tasks[0].ID.String()+"/start", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start task: status = %d, body = %s", resp.StatusCode, body)
	}

	var startResult struct {
		To string `json:"To"`
	}
	if err := json.Unmarshal(body, &startResult); err != nil {
		t.Fatalf("decoding start result: %v", err)
	}

	if startResult.To != "RUNNING" {
		t.Fatalf("task.To after start = %q, want RUNNING", startResult.To)
	}

	// Starting an already-RUNNING task is illegal — must 409, not silently
	// no-op or 500.
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/tasks/"+tasks[0].ID.String()+"/start", adminToken, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("double start: status = %d, want 409", resp.StatusCode)
	}
}

// TestIngestTasksSchemaInvalidWritesNothing proves tasks:ingest's
// no-partial-write contract over real HTTP: a schema-invalid tasks.md must
// 422 with an issue list AND leave the feature with zero tasks.
func TestIngestTasksSchemaInvalidWritesNothing(t *testing.T) {
	ts, _ := newTestServer(t)

	suffix := uuid.NewString()

	projBody, _ := json.Marshal(map[string]any{
		"slug": "proj-bad-" + suffix, "manifest_ref": "main", "manifest_hash": "deadbeef",
	})

	_, body := doJSON(t, http.MethodPost, ts.URL+"/v1/projects", adminToken, projBody)

	var proj struct {
		Slug string `json:"slug"`
	}

	_ = json.Unmarshal(body, &proj)

	featBody, _ := json.Marshal(map[string]any{"slug": "feat-bad-" + suffix})
	_, body = doJSON(t, http.MethodPost, ts.URL+"/v1/projects/"+proj.Slug+"/features", adminToken, featBody)

	var feat struct {
		ID uuid.UUID `json:"id"`
	}

	_ = json.Unmarshal(body, &feat)

	badMD := []byte("```yaml agentfleet-tasks\n" +
		"version: v1\n" +
		"tasks:\n" +
		"  - external_ref: T1\n" +
		"    lane: bogus\n" +
		"    title: x\n" +
		"    intent: y\n" +
		"```\n")

	resp, body := doJSON(t, http.MethodPost, ts.URL+"/v1/features/"+feat.ID.String()+"/tasks:ingest", adminToken, badMD)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body = %s", resp.StatusCode, body)
	}

	var issues struct {
		Issues []struct {
			Message string `json:"message"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &issues); err != nil {
		t.Fatalf("decoding issues: %v", err)
	}

	if len(issues.Issues) == 0 {
		t.Fatal("422 response carried no issues")
	}

	resp, body = doJSON(t, http.MethodGet, ts.URL+"/v1/features/"+feat.ID.String()+"/tasks", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing tasks: status = %d", resp.StatusCode)
	}

	var tasks []any
	if err := json.Unmarshal(body, &tasks); err != nil {
		t.Fatalf("decoding tasks list: %v", err)
	}

	if len(tasks) != 0 {
		t.Fatalf("got %d tasks after a rejected ingest, want 0 — no partial write", len(tasks))
	}
}

// newTestRun creates a project/feature/task/run chain directly through the
// store and returns the run's id and its plaintext bearer token — the
// token authRun expects, never persisted itself (only its sha256 is).
func newTestRun(t *testing.T, st *store.Store) (uuid.UUID, string) {
	t.Helper()

	ctx := t.Context()
	suffix := uuid.NewString()

	proj, err := st.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "run-fixture-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE", Manifest: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := st.Q().CreateFeature(ctx, db.CreateFeatureParams{ProjectID: proj.ID, Slug: "f-" + suffix, State: "OPEN"})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := st.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "t", Intent: "i",
		AcceptanceCriteria: []byte(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
		SpecRefs: []byte(`[]`), State: "CREATED",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	token := "run-token-" + suffix
	sum := sha256.Sum256([]byte(token))

	run, err := st.Q().InsertRun(ctx, db.InsertRunParams{
		TaskID: task.ID, Role: "implementer", Model: "test-model", State: "PENDING", TokenHash: sum[:],
	})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	return run.ID, token
}

// newTestRunningRun is newTestRun's counterpart for handlers that need the
// task already RUNNING (ask_human's own precondition — TrAsked only fires
// from RUNNING) with a caller-chosen role, since ask_human is gated to
// "orchestrator" by the test server's manifest (D7).
func newTestRunningRun(t *testing.T, st *store.Store, role string) (uuid.UUID, string) {
	t.Helper()

	ctx := t.Context()
	suffix := uuid.NewString()

	proj, err := st.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "run-fixture-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE", Manifest: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := st.Q().CreateFeature(ctx, db.CreateFeatureParams{ProjectID: proj.ID, Slug: "f-" + suffix, State: "OPEN"})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := st.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "t", Intent: "i",
		AcceptanceCriteria: []byte(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
		SpecRefs: []byte(`[]`), State: "RUNNING",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	token := "run-token-" + suffix
	sum := sha256.Sum256([]byte(token))

	run, err := st.Q().InsertRun(ctx, db.InsertRunParams{
		TaskID: task.ID, Role: role, Model: "test-model", State: "RUNNING", TokenHash: sum[:],
	})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	return run.ID, token
}

// TestAuthRunBindsTokenToItsOwnRun is the direct regression test for the
// property authRun's doc comment claims but nothing previously verified: a
// valid bearer token for run A must not authenticate a request against run
// B's path, even though both tokens are individually well-formed and
// individually valid for their own run.
func TestAuthRunBindsTokenToItsOwnRun(t *testing.T) {
	ts, st := newTestServer(t)

	runA, tokenA := newTestRun(t, st)
	runB, tokenB := newTestRun(t, st)

	resp, _ := doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+runA.String()+"/checkpoint", tokenA, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("run A's own token against run A: status = %d, want 204", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+runB.String()+"/checkpoint", tokenA, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("run A's token against run B's path: status = %d, want 401", resp.StatusCode)
	}

	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+runA.String()+"/checkpoint", tokenB, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("run B's token against run A's path: status = %d, want 401", resp.StatusCode)
	}
}
