//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
)

// newReviewTask inserts a task directly in REVIEW state (bypassing the
// domain transition machinery, same as newTestRun/newTestRunningRun above —
// these tests exercise the read endpoints, not the state machine).
func newReviewTask(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()

	ctx := t.Context()
	suffix := uuid.NewString()

	proj, err := st.Q().CreateProject(ctx, db.CreateProjectParams{
		Slug: "m7-fixture-" + suffix, ManifestRef: "r", ManifestHash: "h", Repos: []string{}, Status: "ACTIVE", Manifest: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	feat, err := st.Q().CreateFeature(ctx, db.CreateFeatureParams{ProjectID: proj.ID, Slug: "f-" + suffix, State: "OPEN"})
	if err != nil {
		t.Fatalf("CreateFeature: %v", err)
	}

	task, err := st.Q().InsertTask(ctx, db.InsertTaskParams{
		FeatureID: feat.ID, Lane: "direct", Title: "review me", Intent: "i",
		AcceptanceCriteria: []byte(`[]`), Touches: []string{}, DependsOn: []uuid.UUID{},
		SpecRefs: []byte(`[]`), State: "REVIEW",
	})
	if err != nil {
		t.Fatalf("InsertTask: %v", err)
	}

	return task.ID
}

func numericFromString(t *testing.T, s string) pgtype.Numeric {
	t.Helper()

	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		t.Fatalf("pgtype.Numeric.Scan(%q): %v", s, err)
	}

	return n
}

// TestPendingApprovalsIncludesArtifactWhenPresent covers both halves of the
// approval queue's contract: a REVIEW task with an artifact reports its
// sha256, and a REVIEW task without one reports a nil artifact rather than
// erroring (the LEFT JOIN LATERAL nullability bug this endpoint's SQL
// comment documents working around).
func TestPendingApprovalsIncludesArtifactWhenPresent(t *testing.T) {
	ts, st := newTestServer(t)

	withArtifact := newReviewTask(t, st)

	artifact, err := st.Q().InsertArtifact(t.Context(), db.InsertArtifactParams{
		TaskID: withArtifact, Kind: "pr", Uri: "https://github.com/o/r/pull/1", Sha256: "abc123",
	})
	if err != nil {
		t.Fatalf("InsertArtifact: %v", err)
	}

	withoutArtifact := newReviewTask(t, st)

	resp, body := doJSON(t, http.MethodGet, ts.URL+"/v1/approvals/pending", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/approvals/pending: status = %d, body = %s", resp.StatusCode, body)
	}

	var out []struct {
		TaskID   string `json:"task_id"`
		Artifact *struct {
			Kind   string `json:"kind"`
			URI    string `json:"uri"`
			SHA256 string `json:"sha256"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding approvals: %v, body = %s", err, body)
	}

	var foundWith, foundWithout bool

	for _, a := range out {
		switch a.TaskID {
		case withArtifact.String():
			foundWith = true

			if a.Artifact == nil || a.Artifact.SHA256 != artifact.Sha256 {
				t.Errorf("task %s: artifact = %+v, want sha256 %s", a.TaskID, a.Artifact, artifact.Sha256)
			}
		case withoutArtifact.String():
			foundWithout = true

			if a.Artifact != nil {
				t.Errorf("task %s: artifact = %+v, want nil", a.TaskID, a.Artifact)
			}
		}
	}

	if !foundWith {
		t.Errorf("REVIEW task with an artifact missing from /v1/approvals/pending")
	}

	if !foundWithout {
		t.Errorf("REVIEW task without an artifact missing from /v1/approvals/pending")
	}
}

// TestCostMetricsGroupByRoleAndModel drives two runs and checks the total's
// delta — a before/after comparison, since the shared test database
// accumulates rows from every other test.
func TestCostMetricsGroupByRoleAndModel(t *testing.T) {
	ts, st := newTestServer(t)

	before := fetchCost(t, ts)

	run1, _ := newTestRun(t, st)
	run2, _ := newTestRun(t, st)

	if _, err := st.Q().RecordRunUsage(t.Context(), db.RecordRunUsageParams{
		ID: run1, TokensIn: 100, TokensOut: 200, CostUsd: numericFromString(t, "1.5000"),
	}); err != nil {
		t.Fatalf("RecordRunUsage run1: %v", err)
	}

	if _, err := st.Q().RecordRunUsage(t.Context(), db.RecordRunUsageParams{
		ID: run2, TokensIn: 300, TokensOut: 400, CostUsd: numericFromString(t, "2.5000"),
	}); err != nil {
		t.Fatalf("RecordRunUsage run2: %v", err)
	}

	after := fetchCost(t, ts)

	if got, want := after.TotalUSD-before.TotalUSD, 4.0; !almostEqual(got, want) {
		t.Errorf("TotalUSD delta = %v, want %v", got, want)
	}
}

// TestMetricsSummaryReportsFiniteRates checks the summary endpoint's rates
// never come back NaN/Inf — the "0, not NaN" rule metrics.go documents —
// and that its numbers move consistently with a direct policy_violation
// event insert.
func TestMetricsSummaryReportsFiniteRates(t *testing.T) {
	ts, st := newTestServer(t)

	before := fetchSummary(t, ts)

	if before.Drift.Rate != before.Drift.Rate || before.Questions.PerRun != before.Questions.PerRun {
		t.Fatalf("summary rates are NaN before any writes: %+v", before)
	}

	run, _ := newTestRun(t, st)

	var runID pgtype.UUID
	if err := runID.Scan(run.String()); err != nil {
		t.Fatalf("scanning run id: %v", err)
	}

	if _, err := st.Q().InsertControlPlaneEvent(t.Context(), db.InsertControlPlaneEventParams{
		RunID: runID, Kind: "policy_violation", Actor: "control_plane", Payload: []byte(`{}`), Seq: 1,
	}); err != nil {
		t.Fatalf("InsertControlPlaneEvent: %v", err)
	}

	after := fetchSummary(t, ts)

	if after.Violations != before.Violations+1 {
		t.Errorf("Violations = %d, want %d (before + 1)", after.Violations, before.Violations+1)
	}
}

// TestMetricsEndpointsRequireAdminToken is the 401 coverage §4's "every
// cross-cutting read is admin-scoped" convention names — mirrors
// TestAuthAdminRejectsMissingOrWrongToken's shape for the three new routes.
func TestMetricsEndpointsRequireAdminToken(t *testing.T) {
	ts, _ := newTestServer(t)

	for _, path := range []string{"/v1/approvals/pending", "/v1/metrics/cost", "/v1/metrics/summary"} {
		resp, _ := doJSON(t, http.MethodGet, ts.URL+path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with no token: status = %d, want 401", path, resp.StatusCode)
		}
	}
}

func fetchCost(t *testing.T, ts *httptest.Server) costResponseForTest {
	t.Helper()

	resp, body := doJSON(t, http.MethodGet, ts.URL+"/v1/metrics/cost", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/metrics/cost: status = %d, body = %s", resp.StatusCode, body)
	}

	var out costResponseForTest
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding cost response %s: %v", body, err)
	}

	return out
}

func fetchSummary(t *testing.T, ts *httptest.Server) summaryResponseForTest {
	t.Helper()

	resp, body := doJSON(t, http.MethodGet, ts.URL+"/v1/metrics/summary", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/metrics/summary: status = %d, body = %s", resp.StatusCode, body)
	}

	var out summaryResponseForTest
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding summary response %s: %v", body, err)
	}

	return out
}

type costResponseForTest struct {
	TotalUSD float64 `json:"total_usd"`
}

type summaryResponseForTest struct {
	Drift     driftResponse `json:"drift"`
	Questions struct {
		PerRun float64 `json:"per_run"`
	} `json:"questions"`
	Violations int64 `json:"violations"`
}

func almostEqual(a, b float64) bool {
	const eps = 1e-6

	d := a - b
	if d < 0 {
		d = -d
	}

	return d < eps
}
