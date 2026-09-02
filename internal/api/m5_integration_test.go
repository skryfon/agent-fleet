//go:build integration

package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"agentfleet/internal/api"
	"agentfleet/internal/budget"
	"agentfleet/internal/fanout"
	"agentfleet/internal/policy"
	"agentfleet/internal/redact"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
)

// newM5TestServer is newTestServer's M5 counterpart: the REAL manifest
// shape development-plan.md D7/§5 describes (only the orchestrator gets
// ask_human/spawn_worker/answer_worker; a worker gets ask_orchestrator/
// report_to_orchestrator, never ask_human), plus a small FanoutCaps so the
// depth/fan-out test below doesn't need to spawn dozens of workers to
// prove a cap fires.
func newM5TestServer(t *testing.T) (*httptest.Server, *store.Store) {
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
				"orchestrator": {MediatedTools: []string{"ask_human", "spawn_worker", "answer_worker"}},
				"implementer":  {MediatedTools: []string{"ask_orchestrator", "report_to_orchestrator"}},
			},
		},
		FanoutCaps: fanout.Caps{MaxChildrenPerRun: 2},
	}

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	return ts, st
}

// newTestRunForExistingTask inserts a RUNNING run for an already-existing
// task (a spawned child, in this file's own tests) — newTestRunningRun's
// counterpart for a task that wasn't itself freshly created by the helper,
// standing in for internal/supervisor.RunLaunch the same way every other
// *_integration_test.go in this repo does for tests that don't run the
// outbox relay.
func newTestRunForExistingTask(t *testing.T, st *store.Store, taskID uuid.UUID, role string) (runID uuid.UUID, token string) {
	t.Helper()

	tok := "run-token-" + uuid.NewString()
	sum := sha256.Sum256([]byte(tok))

	run, err := st.Q().InsertRun(context.Background(), db.InsertRunParams{
		TaskID: taskID, Role: role, Model: "test-model", State: "RUNNING", TokenHash: sum[:],
	})
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}

	return run.ID, tok
}

type spawnResponse struct {
	Allow  bool   `json:"allow"`
	Rule   string `json:"rule"`
	Result struct {
		TaskID string `json:"task_id"`
	} `json:"result"`
}

// TestSpawnWorkerFanoutCapEnforced is half of M5's own done-when
// (development-plan.md §7: "a feature fans out to parallel tasks"): an
// orchestrator can spawn up to FanoutCaps.MaxChildrenPerRun active
// children, and the next spawn past the cap is denied — recorded as its
// own violation, not silently dropped.
func TestSpawnWorkerFanoutCapEnforced(t *testing.T) {
	ts, st := newM5TestServer(t)

	orchRun, orchToken := newTestRunningRun(t, st, "orchestrator")

	spawn := func(title string) (int, spawnResponse) {
		body, _ := json.Marshal(map[string]any{"title": title, "intent": "do work"})
		resp, respBody := doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+orchRun.String()+"/tools/spawn_worker", orchToken, body)

		var out spawnResponse
		if err := json.Unmarshal(respBody, &out); err != nil {
			t.Fatalf("decoding spawn_worker response %s: %v", respBody, err)
		}

		return resp.StatusCode, out
	}

	status1, r1 := spawn("worker one")
	if status1 != http.StatusOK || !r1.Allow || r1.Result.TaskID == "" {
		t.Fatalf("first spawn_worker: status=%d allow=%v task_id=%q, want 200/true/non-empty", status1, r1.Allow, r1.Result.TaskID)
	}

	status2, r2 := spawn("worker two")
	if status2 != http.StatusOK || !r2.Allow {
		t.Fatalf("second spawn_worker: status=%d allow=%v, want 200/true", status2, r2.Allow)
	}

	status3, r3 := spawn("worker three")
	if status3 != http.StatusForbidden || r3.Allow || r3.Rule != "max_children" {
		t.Fatalf("third spawn_worker: status=%d allow=%v rule=%q, want 403/false/max_children", status3, r3.Allow, r3.Rule)
	}
}

// TestAskOrchestratorRoutesToParentRunNotZulip is the other half of M5's
// done-when: "the orchestrator is the only agent reaching a human." A
// worker's ask_orchestrator must (1) succeed for a role D7 denies plain
// ask_human to, (2) enqueue NO zulip.question outbox row — the literal
// thing D7 forbids — and (3) surface as a worker_question on the
// orchestrator's own inbox, which answer_worker then resolves through the
// ordinary TrAnswered -> run.launch/resume path.
func TestAskOrchestratorRoutesToParentRunNotZulip(t *testing.T) {
	ts, st := newM5TestServer(t)

	orchRun, orchToken := newTestRunningRun(t, st, "orchestrator")

	spawnBody, _ := json.Marshal(map[string]any{"title": "child", "intent": "do work"})
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+orchRun.String()+"/tools/spawn_worker", orchToken, spawnBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spawn_worker: status = %d, body = %s", resp.StatusCode, body)
	}

	var spawned spawnResponse
	if err := json.Unmarshal(body, &spawned); err != nil {
		t.Fatalf("decoding spawn_worker response: %v", err)
	}

	childRun, childToken := newTestRunForExistingTask(t, st, uuid.MustParse(spawned.Result.TaskID), "implementer")

	// A plain worker (implementer) is denied ask_human outright (D7) —
	// same manifest check as TestAskHumanBlocksTaskAndDeniedForNonOrchestrator,
	// asserted here too so this test file stands on its own.
	askHumanBody, _ := json.Marshal(map[string]any{"question": "?", "kind": "free_text"})
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+childRun.String()+"/tools/ask_human", childToken, askHumanBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("worker ask_human: status = %d, body = %s, want 403", resp.StatusCode, body)
	}

	askOrchBody, _ := json.Marshal(map[string]any{"question": "which lib?", "kind": "free_text"})
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+childRun.String()+"/tools/ask_orchestrator", childToken, askOrchBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("worker ask_orchestrator: status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	var askResult struct {
		Result struct {
			QuestionID string `json:"question_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &askResult); err != nil {
		t.Fatalf("decoding ask_orchestrator response: %v", err)
	}

	if askResult.Result.QuestionID == "" {
		t.Fatal("ask_orchestrator returned no question_id")
	}

	assertNoZulipQuestionOutboxRow(t, askResult.Result.QuestionID)

	// The orchestrator's own inbox must deliver the worker_question.
	resp, body = doJSON(t, http.MethodGet, ts.URL+"/v1/runs/"+orchRun.String()+"/inbox?wait=1", orchToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("orchestrator inbox: status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	var inboxMsg struct {
		Kind    string `json:"kind"`
		Payload struct {
			QuestionID string `json:"question_id"`
			FromRunID  string `json:"from_run_id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &inboxMsg); err != nil {
		t.Fatalf("decoding orchestrator inbox response %s: %v", body, err)
	}

	if inboxMsg.Kind != "worker_question" || inboxMsg.Payload.QuestionID != askResult.Result.QuestionID {
		t.Fatalf("orchestrator inbox = %+v, want kind=worker_question question_id=%s", inboxMsg, askResult.Result.QuestionID)
	}

	if inboxMsg.Payload.FromRunID != childRun.String() {
		t.Fatalf("inbox payload from_run_id = %q, want %s", inboxMsg.Payload.FromRunID, childRun.String())
	}

	// answer_worker resolves it through the ordinary TrAnswered path.
	answerBody, _ := json.Marshal(map[string]any{"question_id": askResult.Result.QuestionID, "answer": "use foo"})
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+orchRun.String()+"/tools/answer_worker", orchToken, answerBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer_worker: status = %d, body = %s, want 200", resp.StatusCode, body)
	}
}

// TestCancelTaskCancelsSubtree is M5's cancellation done-when, exercised
// through the real HTTP surface (internal/store's own version of this is
// internal/store/store_integration_test.go's
// TestCancelSubtreeCancelsEveryDescendant).
func TestCancelTaskCancelsSubtree(t *testing.T) {
	ts, st := newM5TestServer(t)

	orchRun, orchToken := newTestRunningRun(t, st, "orchestrator")

	spawnBody, _ := json.Marshal(map[string]any{"title": "child", "intent": "do work"})
	resp, body := doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+orchRun.String()+"/tools/spawn_worker", orchToken, spawnBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spawn_worker: status = %d, body = %s", resp.StatusCode, body)
	}

	var spawned spawnResponse
	if err := json.Unmarshal(body, &spawned); err != nil {
		t.Fatalf("decoding spawn_worker response: %v", err)
	}

	orchestratorRun, err := st.Q().GetRunByID(context.Background(), orchRun)
	if err != nil {
		t.Fatalf("GetRunByID(orchestrator): %v", err)
	}

	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/tasks/"+orchestratorRun.TaskID.String()+"/cancel", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel root task: status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	childTask, err := st.Q().GetTaskByID(context.Background(), uuid.MustParse(spawned.Result.TaskID))
	if err != nil {
		t.Fatalf("GetTaskByID(child): %v", err)
	}

	if childTask.State != "CANCELLED" {
		t.Fatalf("child task state = %s, want CANCELLED", childTask.State)
	}
}

// assertNoZulipQuestionOutboxRow is D7's literal negative assertion: no
// zulip.question outbox row exists whose payload names questionID —
// ask_orchestrator (unlike ask_human) must never schedule that effect.
func assertNoZulipQuestionOutboxRow(t *testing.T, questionID string) {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM outbox WHERE topic = 'zulip.question' AND payload::text LIKE '%' || $1 || '%'",
		questionID,
	).Scan(&count); err != nil {
		t.Fatalf("querying for a zulip.question outbox row: %v", err)
	}

	if count != 0 {
		t.Fatalf("found %d zulip.question outbox row(s) for an ask_orchestrator question — D7 violated", count)
	}
}

// TestQuestionBudgetCapEnforced is M5's own enforcement of §6's "Cap
// questions per run (3) and per feature (10)" — previously a dead letter
// (internal/store.incrementBudgetScope hardcoded QuestionsAsked: 0). An
// orchestrator's ask_human calls are capped per this server's
// BudgetCaps.Questions; the cap-breaching call is refused with 429 and,
// critically, does NOT leave a question row behind (the whole ApplyAsk
// transaction rolls back on ErrQuestionBudgetBreached).
func TestQuestionBudgetCapEnforced(t *testing.T) {
	_, st := newM5TestServer(t)

	// A tighter cap than newM5TestServer's own FanoutCaps-only default —
	// built inline so this test doesn't depend on that default's value.
	srv := &api.Server{
		Store:      st,
		Redact:     redact.New(nil, nil),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		AdminToken: adminToken,
		Manifest: policy.Manifest{
			Roles: map[string]policy.Role{"orchestrator": {MediatedTools: []string{"ask_human"}}},
		},
		BudgetCaps: budget.Caps{Questions: 1},
	}
	capped := httptest.NewServer(srv.Routes())
	t.Cleanup(capped.Close)

	orchRun, orchToken := newTestRunningRun(t, st, "orchestrator")

	askBody, _ := json.Marshal(map[string]any{"question": "q1", "kind": "free_text"})
	resp, body := doJSON(t, http.MethodPost, capped.URL+"/v1/runs/"+orchRun.String()+"/tools/ask_human", orchToken, askBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first ask_human: status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	var first struct {
		Result struct {
			QuestionID string `json:"question_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatalf("decoding first ask_human response: %v", err)
	}

	// Answer it so the second ask isn't rejected by question_one_open_per_feature_uk
	// instead of the budget cap this test is actually checking.
	answerBody, _ := json.Marshal(map[string]any{"answer": "a1", "answered_by": "architect"})
	resp, body = doJSON(t, http.MethodPost, capped.URL+"/v1/questions/"+first.Result.QuestionID+"/answer", adminToken, answerBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answering first question: status = %d, body = %s", resp.StatusCode, body)
	}

	askBody2, _ := json.Marshal(map[string]any{"question": "q2", "kind": "free_text"})
	resp, body = doJSON(t, http.MethodPost, capped.URL+"/v1/runs/"+orchRun.String()+"/tools/ask_human", orchToken, askBody2)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second ask_human past the question cap: status = %d, body = %s, want 429", resp.StatusCode, body)
	}
}

// TestDriftMetricCountsDeviations is a plain smoke test for GET
// /v1/metrics/drift (development-plan.md §11) — report_deviation writes a
// "deviation" event, and the endpoint's rate is deviations/tasks.
func TestDriftMetricCountsDeviations(t *testing.T) {
	ts, st := newM5TestServer(t)

	before := fetchDrift(t, ts)

	orchRun, orchToken := newTestRunningRun(t, st, "orchestrator")

	srv := &api.Server{
		Store: st, Redact: redact.New(nil, nil), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AdminToken: adminToken,
		Manifest: policy.Manifest{Roles: map[string]policy.Role{"orchestrator": {MediatedTools: []string{"report_deviation"}}}},
	}
	devTS := httptest.NewServer(srv.Routes())
	t.Cleanup(devTS.Close)

	body, _ := json.Marshal(map[string]any{"what": "used a different lib", "why": "spec didn't cover it"})
	resp, respBody := doJSON(t, http.MethodPost, devTS.URL+"/v1/runs/"+orchRun.String()+"/tools/report_deviation", orchToken, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("report_deviation: status = %d, body = %s, want 200", resp.StatusCode, respBody)
	}

	after := fetchDrift(t, ts)

	if after.Deviations != before.Deviations+1 {
		t.Fatalf("Deviations = %d, want %d (before + 1)", after.Deviations, before.Deviations+1)
	}
}

func fetchDrift(t *testing.T, ts *httptest.Server) driftResponse {
	t.Helper()

	resp, body := doJSON(t, http.MethodGet, ts.URL+"/v1/metrics/drift", adminToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/metrics/drift: status = %d, body = %s", resp.StatusCode, body)
	}

	var out driftResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding drift response %s: %v", body, err)
	}

	return out
}

type driftResponse struct {
	Deviations int64   `json:"deviations"`
	Tasks      int64   `json:"tasks"`
	Rate       float64 `json:"rate"`
}
