package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentfleet/internal/policy"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
)

type dispatchToolResponse struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
	Rule   string `json:"rule,omitempty"`
	// Result carries a tool's own return value for the handful of mediated
	// tools that do work after an allow (M3's ask_human is the first — see
	// askHumanArgs below). Every other mediated tool's dispatch stops at
	// recording the decision (M2's documented stopping point), so Result is
	// omitted for them.
	Result json.RawMessage `json:"result,omitempty"`
}

// askHumanArgs is ask_human's argument shape (development-plan.md §6:
// "ask_human(question, kind, options?, context_ref?)"). context_ref is
// accepted (ignored by encoding/json, not declared here) — D8's hash-pinned
// context resolution is af-context's job, not this handler's.
type askHumanArgs struct {
	Question  string   `json:"question"`
	Kind      string   `json:"kind"`
	Options   []string `json:"options,omitempty"`
	Addressee *string  `json:"addressee,omitempty"`
}

type askHumanResult struct {
	QuestionID string `json:"question_id"`
}

// prOpenedArgs is af-github's post-creation report (runner/packages/
// af-github, M4) — the sha256 POST /v1/approvals will later be asked to
// match.
type prOpenedArgs struct {
	URL        string `json:"url"`
	HeadSHA    string `json:"head_sha"`
	DiffSHA256 string `json:"diff_sha256"`
	Base       string `json:"base"`
}

// dispatchTool is the mediated-tool-dispatch endpoint
// (development-plan.md §4: "Mediated tools ... go through the API so the
// decision is recorded as an event"). The policy decision — allow or deny —
// is recorded as a control-plane event before this handler returns. M2
// stops at recording the decision: actually executing an allowed mediated
// tool (spawning a subagent, creating a PR) is M4/M5 scope layered on top
// of this same endpoint, not a new one.
func (s *Server) dispatchTool(w http.ResponseWriter, r *http.Request) {
	run, _ := runFromContext(r.Context())
	tool := r.PathValue("name")

	args, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())

		return
	}

	decision := policy.Evaluate(policy.Request{
		Role: run.Role, Tool: tool, Args: args, Manifest: s.Manifest,
	})

	// Flagged in DB review: unlike every transition-writing path,
	// RecordEvent originally had no way to dedupe a client-side retry (a
	// tool-dispatch client retrying a POST whose response was lost), which
	// would otherwise mint a second, distinct audit event via a fresh
	// IncrementRunEventSeq call instead of a no-op. Same Idempotency-Key
	// header convention applyTaskTrigger (tasks.go) already uses for task
	// lifecycle POSTs.
	var dedupeKey *string
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		dedupeKey = &k
	}

	if !decision.Allow {
		// RecordViolation (not plain RecordEvent) so the denial reaches
		// Zulip (development-plan.md §4 M4) — a mediated deny is the
		// control-plane's OWN policy.Evaluate saying no, source
		// "control_plane"; af-policy's runner-side deny (a different tool
		// entirely, never reaching this handler) reports itself via
		// POST /v1/runs/{id}/violations instead (violations.go).
		if _, err := s.Store.RecordViolation(r.Context(), s.Redact, run.ID, tool, decision.Reason, "control_plane", dedupeKey); err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}

		writeJSON(w, http.StatusForbidden, dispatchToolResponse{Allow: false, Reason: decision.Reason, Rule: decision.Rule})

		return
	}

	payload := map[string]any{"tool": tool, "rule": decision.Rule}

	if _, err := s.Store.RecordEvent(r.Context(), s.Redact, run.ID, "tool_dispatch_allowed", payload, dedupeKey); err != nil {
		writeTransitionErr(w, s.Log, err)

		return
	}

	switch tool {
	case "ask_human":
		result, err := s.askHuman(r, run, args)
		if err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}

		resultJSON, err := json.Marshal(result)
		if err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}

		writeJSON(w, http.StatusOK, dispatchToolResponse{Allow: true, Rule: decision.Rule, Result: resultJSON})

		return
	case "pr_opened":
		if err := s.prOpened(r, run, args); err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}
	}

	writeJSON(w, http.StatusOK, dispatchToolResponse{Allow: true, Rule: decision.Rule})
}

// askHuman is dispatchTool's post-allow action for the one mediated tool M3
// gives real behavior: it inserts the question row, transitions the task to
// BLOCKED_ON_HUMAN, and enqueues the zulip.question notification, all via
// Store.ApplyAsk's single transaction. A malformed body, an unrecognized
// kind, or a feature that already has an open question all return through
// writeTransitionErr's mapping (store.ErrQuestionAlreadyOpen -> 409) rather
// than a bare 500.
func (s *Server) askHuman(r *http.Request, run db.Run, rawArgs []byte) (askHumanResult, error) {
	var args askHumanArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return askHumanResult{}, fmt.Errorf("ask_human: parsing arguments: %w", err)
	}

	options := []byte(`[]`)
	if len(args.Options) > 0 {
		marshaled, err := json.Marshal(args.Options)
		if err != nil {
			return askHumanResult{}, fmt.Errorf("ask_human: marshaling options: %w", err)
		}

		options = marshaled
	}

	result, err := s.Store.ApplyAsk(r.Context(), s.Redact, store.AskRequest{
		RunID: run.ID, TaskID: run.TaskID,
		Kind: args.Kind, Body: args.Question, Options: options, Addressee: args.Addressee,
		Actor: "run:" + run.ID.String(),
	})
	if err != nil {
		return askHumanResult{}, err
	}

	return askHumanResult{QuestionID: result.Question.ID.String()}, nil
}

// prOpened is dispatchTool's post-allow action for M4's second mediated
// tool with real behavior: it records the artifact af-github's gh_pr_create
// just opened so POST /v1/approvals has a sha256 to bind an approval to
// (development-plan.md §3: "approval.subject_sha256 is mandatory"). Unlike
// askHuman it doesn't drive a task transition itself — the task is already
// heading to REVIEW via the run's own eventual exit (TrRunExitedOK); this
// only has to exist BEFORE that REVIEW notification goes out, which is
// af-github's own ordering (dispatch pr_opened, then exit).
func (s *Server) prOpened(r *http.Request, run db.Run, rawArgs []byte) error {
	var args prOpenedArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fmt.Errorf("pr_opened: parsing arguments: %w", err)
	}

	_, err := s.Store.Q().InsertArtifact(r.Context(), db.InsertArtifactParams{
		TaskID: run.TaskID, Kind: "pr", Uri: args.URL, Sha256: args.DiffSHA256,
	})
	if err != nil {
		return fmt.Errorf("pr_opened: recording artifact: %w", err)
	}

	return nil
}
