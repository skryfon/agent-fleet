package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/fanout"
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

// spawnWorkerArgs is af-subagent's spawn_worker argument shape
// (development-plan.md §5/§7 M5): a title/intent/acceptance_criteria triple
// identical in shape to a tasks.md task, plus an optional role override.
type spawnWorkerArgs struct {
	Title              string   `json:"title"`
	Intent             string   `json:"intent"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	Role               string   `json:"role,omitempty"`
}

type spawnWorkerResult struct {
	TaskID string `json:"task_id"`
}

// answerWorkerArgs is af-subagent's answer_worker argument shape — the
// orchestrator's side of D7's ask_orchestrator round trip (M5).
type answerWorkerArgs struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
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
	case "spawn_worker":
		result, fanoutDecision, err := s.spawnWorker(r, run, args, dedupeKey)
		if err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}

		if !fanoutDecision.Allow {
			// The role's manifest already allowed dispatching spawn_worker at
			// all (decision.Allow above) — this is a SEPARATE, resource-shaped
			// denial (depth/fan-out caps), recorded as its own violation event
			// so it reaches Zulip the same way a policy deny does.
			if _, err := s.Store.RecordViolation(r.Context(), s.Redact, run.ID, tool, fanoutDecision.Reason, "control_plane", nil); err != nil {
				writeTransitionErr(w, s.Log, err)

				return
			}

			writeJSON(w, http.StatusForbidden, dispatchToolResponse{Allow: false, Reason: fanoutDecision.Reason, Rule: fanoutDecision.Rule})

			return
		}

		resultJSON, err := json.Marshal(result)
		if err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}

		writeJSON(w, http.StatusOK, dispatchToolResponse{Allow: true, Rule: decision.Rule, Result: resultJSON})

		return
	case "ask_orchestrator":
		result, err := s.askOrchestrator(r, run, args)
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
	case "answer_worker":
		if err := s.answerWorker(r, run, args); err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}
	case "report_deviation":
		if err := s.reportDeviation(r, run, args); err != nil {
			writeTransitionErr(w, s.Log, err)

			return
		}
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
		Actor: "run:" + run.ID.String(), Caps: s.BudgetCaps,
	})
	if err != nil {
		return askHumanResult{}, err
	}

	return askHumanResult{QuestionID: result.Question.ID.String()}, nil
}

// spawnWorker is dispatchTool's post-allow action for M5's spawn_worker: it
// loads the spawning run's own task (for its current depth and active
// child/subtree counts), evaluates internal/fanout.Check against
// s.FanoutCaps, and — only on allow — creates the child task via
// Store.ApplySpawn. A fanout denial is reported back to the caller (which
// records it as its own violation event, mirroring a policy deny) rather
// than as an error, since "the caps say no" is an ordinary, expected
// outcome, not a failure.
func (s *Server) spawnWorker(r *http.Request, run db.Run, rawArgs []byte, dedupeKey *string) (spawnWorkerResult, fanout.Decision, error) {
	var args spawnWorkerArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return spawnWorkerResult{}, fanout.Decision{}, fmt.Errorf("spawn_worker: parsing arguments: %w", err)
	}

	ctx := r.Context()

	parentTask, err := s.Store.Q().GetTaskByID(ctx, run.TaskID)
	if err != nil {
		return spawnWorkerResult{}, fanout.Decision{}, fmt.Errorf("spawn_worker: loading parent task %s: %w", run.TaskID, err)
	}

	siblings, err := s.Store.Q().CountActiveChildTasksForRun(ctx, pgtype.UUID{Bytes: run.ID, Valid: true})
	if err != nil {
		return spawnWorkerResult{}, fanout.Decision{}, fmt.Errorf("spawn_worker: counting active children for run %s: %w", run.ID, err)
	}

	// activeSubtree counts the PROSPECTIVE total (internal/fanout.Check's own
	// doc comment) — this spawn's own new child included — so +1 rather than
	// the pre-spawn count. parentTask is the walk root: CountActiveSubtreeTasks'
	// own recursive CTE already walks every task transitively spawned under it.
	subtree, err := s.Store.Q().CountActiveSubtreeTasks(ctx, parentTask.ID)
	if err != nil {
		return spawnWorkerResult{}, fanout.Decision{}, fmt.Errorf("spawn_worker: counting active subtree for task %s: %w", parentTask.ID, err)
	}

	fanoutDecision := fanout.Check(int(parentTask.Depth)+1, int(siblings), int(subtree)+1, s.FanoutCaps)
	if !fanoutDecision.Allow {
		return spawnWorkerResult{}, fanoutDecision, nil
	}

	acceptanceCriteria := []byte(`[]`)
	if len(args.AcceptanceCriteria) > 0 {
		marshaled, err := json.Marshal(args.AcceptanceCriteria)
		if err != nil {
			return spawnWorkerResult{}, fanout.Decision{}, fmt.Errorf("spawn_worker: marshaling acceptance_criteria: %w", err)
		}

		acceptanceCriteria = marshaled
	}

	result, err := s.Store.ApplySpawn(ctx, s.Redact, store.SpawnRequest{
		ParentTaskID: run.TaskID, ParentRunID: run.ID,
		Title: args.Title, Intent: args.Intent, AcceptanceCriteria: acceptanceCriteria,
		Role: args.Role, Actor: "run:" + run.ID.String(), DedupeKey: dedupeKey,
	})
	if err != nil {
		return spawnWorkerResult{}, fanout.Decision{}, err
	}

	return spawnWorkerResult{TaskID: result.Child.ID.String()}, fanout.Decision{Allow: true}, nil
}

// askOrchestrator is dispatchTool's post-allow action for D7's M5
// ask_orchestrator: it resolves the calling run's task.parent_run_id (the
// run that spawned it — nil for a run whose task was never spawned, which
// is a 400: there is no orchestrator to ask) and calls Store.ApplyAsk with
// ToRunID set, so it fires TrAskedOrchestrator instead of TrAsked — no
// zulip.question effect, and a worker_question lands on the orchestrator's
// run_inbox instead (D7: a worker never reaches Zulip directly).
func (s *Server) askOrchestrator(r *http.Request, run db.Run, rawArgs []byte) (askHumanResult, error) {
	var args askHumanArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return askHumanResult{}, fmt.Errorf("ask_orchestrator: parsing arguments: %w", err)
	}

	ctx := r.Context()

	task, err := s.Store.Q().GetTaskByID(ctx, run.TaskID)
	if err != nil {
		return askHumanResult{}, fmt.Errorf("ask_orchestrator: loading task %s: %w", run.TaskID, err)
	}

	if !task.ParentRunID.Valid {
		return askHumanResult{}, errNoOrchestrator
	}

	toRunID := uuid.UUID(task.ParentRunID.Bytes)

	options := []byte(`[]`)
	if len(args.Options) > 0 {
		marshaled, err := json.Marshal(args.Options)
		if err != nil {
			return askHumanResult{}, fmt.Errorf("ask_orchestrator: marshaling options: %w", err)
		}

		options = marshaled
	}

	result, err := s.Store.ApplyAsk(ctx, s.Redact, store.AskRequest{
		RunID: run.ID, TaskID: run.TaskID,
		Kind: args.Kind, Body: args.Question, Options: options, Addressee: args.Addressee,
		Actor: "run:" + run.ID.String(), ToRunID: &toRunID, Caps: s.BudgetCaps,
	})
	if err != nil {
		return askHumanResult{}, err
	}

	return askHumanResult{QuestionID: result.Question.ID.String()}, nil
}

// errNoOrchestrator is askOrchestrator's sentinel for a run whose task was
// never spawned (task.parent_run_id is NULL) — a 400, not a 500: the
// caller asked a question with nobody to route it to.
var errNoOrchestrator = errors.New("api: this run has no orchestrator to ask (its task was not spawned by another run)")

// answerWorkerArgs -> answerWorker is answer_worker's post-allow action —
// the orchestrator's side of D7's ask_orchestrator round trip. It verifies
// the question is actually addressed to the calling run (to_run_id) before
// answering it, so an orchestrator cannot answer a sibling orchestrator's
// worker's question by guessing its id, then reuses the exact same
// Store.ApplyAnswer path M3's human answerQuestion handler uses — the
// resulting TrAnswered -> run.launch/resume effect resurrects the worker
// and af-resume injects the answer, no new resume machinery required.
func (s *Server) answerWorker(r *http.Request, run db.Run, rawArgs []byte) error {
	var args answerWorkerArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fmt.Errorf("answer_worker: parsing arguments: %w", err)
	}

	questionID, err := uuid.Parse(args.QuestionID)
	if err != nil {
		return fmt.Errorf("answer_worker: invalid question_id %q: %w", args.QuestionID, err)
	}

	ctx := r.Context()

	question, err := s.Store.Q().GetQuestionByID(ctx, questionID)
	if err != nil {
		return fmt.Errorf("answer_worker: loading question %s: %w", questionID, err)
	}

	if !question.ToRunID.Valid || question.ToRunID.Bytes != run.ID {
		return errNotAddressedToCaller
	}

	_, err = s.Store.ApplyAnswer(ctx, s.Redact, store.AnswerRequest{
		QuestionID: questionID, Answer: args.Answer, AnsweredBy: "run:" + run.ID.String(),
		Actor: "run:" + run.ID.String(),
	})

	return err
}

// errNotAddressedToCaller is answerWorker's sentinel for a question that
// exists but was not addressed to the calling run — an authorization
// failure (writeTransitionErr maps it to 403), not a state conflict. A
// well-behaved orchestrator only ever answers question ids its own
// run_inbox handed it, so this path is a bug signal, not normal traffic.
var errNotAddressedToCaller = errors.New("api: this question is not addressed to the calling run")

// deviationArgs is af-subagent's report_deviation argument shape
// (development-plan.md §5/§7 M5, §11's drift-rate metric).
type deviationArgs struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

// reportDeviation is dispatchTool's post-allow action for M5's
// report_deviation: it writes a plain "deviation" event via the same
// Store.RecordEvent every other mediated tool's own tool_dispatch_allowed
// event already goes through — no task transition, no outbox effect. §11's
// drift-rate metric (GET /v1/metrics/drift) counts these directly off the
// append-only event log rather than a denormalised counter column.
func (s *Server) reportDeviation(r *http.Request, run db.Run, rawArgs []byte) error {
	var args deviationArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fmt.Errorf("report_deviation: parsing arguments: %w", err)
	}

	_, err := s.Store.RecordEvent(r.Context(), s.Redact, run.ID, "deviation", map[string]any{"what": args.What, "why": args.Why}, nil)

	return err
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
