//go:build integration

package api_test

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
)

// newTestRunWithManifest is newTestRun's counterpart for M6: a project
// registered with a real compiled manifest (rather than the '{}' no-manifest
// default every other fixture in this package uses), so a test can assert
// that TWO projects' runs are evaluated against THEIR OWN manifests, not a
// shared process-wide one — the M6 done-criterion
// (development-plan.md §7: "a second project onboards ... zero framework
// code changes") as an executable test.
func newTestRunWithManifest(t *testing.T, st *store.Store, role string, compiledManifest []byte) (uuid.UUID, string) {
	t.Helper()

	ctx := t.Context()
	suffix := uuid.NewString()

	proj, err := st.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "manifest-fixture-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE",
		Manifest: compiledManifest,
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
		TaskID: task.ID, Role: role, Model: "test-model", State: "PENDING", TokenHash: sum[:],
	})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	return run.ID, token
}

// TestPerProjectManifestGatesToolsIndependently is M6's own done-criterion,
// exercised end to end through the real dispatch endpoint: project A's
// manifest allows gh_pr_create for "implementer", project B's manifest
// does not — the SAME tool, the SAME role name, two different projects,
// two different outcomes. Nothing but each project's own stored manifest
// (internal/api.resolveManifest) explains the difference.
func TestPerProjectManifestGatesToolsIndependently(t *testing.T) {
	ts, st := newTestServer(t)

	manifestA := []byte(`{"agents":{"implementer":{"model":"deepseek-v4-pro","tools":["gh_pr_create"]}}}`)
	manifestB := []byte(`{"agents":{"implementer":{"model":"deepseek-v4-pro","tools":["read"]}}}`)

	runA, tokenA := newTestRunWithManifest(t, st, "implementer", manifestA)
	runB, tokenB := newTestRunWithManifest(t, st, "implementer", manifestB)

	prBody, _ := json.Marshal(map[string]any{
		"url": "https://github.com/example/repo/pull/1", "head_sha": "deadbeef", "diff_sha256": "cafef00d", "base": "main",
	})

	respA, bodyA := doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+runA.String()+"/tools/gh_pr_create", tokenA, prBody)
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("project A (manifest allows gh_pr_create): status = %d, body = %s, want 200", respA.StatusCode, bodyA)
	}

	respB, bodyB := doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+runB.String()+"/tools/gh_pr_create", tokenB, prBody)
	if respB.StatusCode != http.StatusForbidden {
		t.Fatalf("project B (manifest does not list gh_pr_create): status = %d, body = %s, want 403", respB.StatusCode, bodyB)
	}
}
