//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestAskHumanBlocksTaskAndDeniedForNonOrchestrator exercises Phase 2's
// happy path AND D7 (docs/adr/0007: "only the orchestrator gets ask_human")
// through the real mediated-tool-dispatch endpoint, not internal/policy
// directly.
func TestAskHumanBlocksTaskAndDeniedForNonOrchestrator(t *testing.T) {
	ts, st := newTestServer(t)

	// A non-orchestrator role is denied by the manifest itself (D7) — the
	// SAME dispatch path an orchestrator succeeds through below, no
	// ask_human-specific check anywhere.
	implRun, implToken := newTestRunningRun(t, st, "implementer")

	askBody, _ := json.Marshal(map[string]any{"question": "which branch?", "kind": "free_text"})

	resp, body := doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+implRun.String()+"/tools/ask_human", implToken, askBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("implementer ask_human: status = %d, body = %s, want 403", resp.StatusCode, body)
	}

	orchRun, orchToken := newTestRunningRun(t, st, "orchestrator")

	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+orchRun.String()+"/tools/ask_human", orchToken, askBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("orchestrator ask_human: status = %d, body = %s, want 200", resp.StatusCode, body)
	}

	var dispatch struct {
		Allow  bool `json:"allow"`
		Result struct {
			QuestionID string `json:"question_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &dispatch); err != nil {
		t.Fatalf("decoding dispatch response: %v", err)
	}

	if !dispatch.Allow || dispatch.Result.QuestionID == "" {
		t.Fatalf("dispatch response = %s, want allow=true and a non-empty question_id", body)
	}

	// A second ask against the same feature while the first is still OPEN
	// must 409 — question_one_open_per_feature_uk surfaced end-to-end.
	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/runs/"+orchRun.String()+"/tools/ask_human", orchToken, askBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second concurrent ask_human: status = %d, body = %s, want 409", resp.StatusCode, body)
	}

	// POST /v1/questions/{id}/answer unblocks the task.
	answerBody, _ := json.Marshal(map[string]any{"answer": "main", "answered_by": "architect"})

	resp, body = doJSON(t, http.MethodPost, ts.URL+"/v1/questions/"+dispatch.Result.QuestionID+"/answer", adminToken, answerBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer question: status = %d, body = %s", resp.StatusCode, body)
	}

	var answered struct {
		TaskState string `json:"task_state"`
	}
	if err := json.Unmarshal(body, &answered); err != nil {
		t.Fatalf("decoding answer response: %v", err)
	}

	if answered.TaskState != "RUNNING" {
		t.Fatalf("task_state after answer = %q, want RUNNING", answered.TaskState)
	}

	// Answering the same question again must 409, not silently re-fire.
	resp, _ = doJSON(t, http.MethodPost, ts.URL+"/v1/questions/"+dispatch.Result.QuestionID+"/answer", adminToken, answerBody)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-answering: status = %d, want 409", resp.StatusCode)
	}
}
