package supervisor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"agentfleet/internal/domain/manifest"
	"agentfleet/internal/domain/prompts"
	"agentfleet/internal/outbox"
	db "agentfleet/internal/store/gen"
)

// effectPayload mirrors internal/store/transition.go's unexported
// effectPayload{TaskID, RunID, QuestionID} — the JSON body every
// run.launch/run.kill outbox row carries. RunID is empty on a first launch
// (no run row exists yet); a resurrect-and-resume launch (M3) carries the
// JUST-EXITED run's id (Store.ApplyAnswer's own doc comment), and
// QuestionID names the question that was answered.
type effectPayload struct {
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
}

// Store is the subset of *internal/store.Store these handlers need — kept
// narrow (like internal/outbox.Store) so a fake can stand in for tests
// without a real Postgres.
type Store interface {
	Q() *db.Queries
	// CheckPause gates RunLaunch on the M4 kill switch — see this method's
	// own doc comment on internal/store.Store for why only "global" is
	// actually checked.
	CheckPause(ctx context.Context, scope string) error
}

// Handlers holds run.launch/run.kill's dependencies. Register both with an
// internal/outbox.Relay before calling Relay.Run — see cmd/control-plane's
// main.go.
type Handlers struct {
	Store  Store
	Daemon *Client
	// RunTokenSecret derives each run's bearer token deterministically as
	// HMAC-SHA256(RunTokenSecret, run_id) instead of storing random bytes
	// anywhere. A run's id is generated here (not left to Postgres's
	// gen_random_uuid() default) specifically so the token can be derived
	// BEFORE the INSERT and re-derived identically on a redelivered
	// run.launch (the INSERT succeeded on a prior attempt but the daemon
	// call never landed) — no plaintext-token storage or second secret
	// channel needed to make the daemon call idempotent. In practice this
	// is SUPERVISOR_SECRET, already shared between control-plane and the
	// daemon for the container-report callback's auth.
	RunTokenSecret string
	// DefaultRole/DefaultModel are the FALLBACK role/model for a project
	// whose manifest declares no agents (M6) — development-plan.md §5's
	// model-allocation table becomes real per task/role via a project's own
	// manifest (internal/domain/manifest.Manifest), resolved in RunLaunch;
	// these two remain the no-manifest fallback, same role as
	// api.Server.Manifest/BudgetCaps/FanoutCaps.
	DefaultRole  string
	DefaultModel string
}

// runToken derives runID's bearer token. sha256(runToken(...)) is what gets
// stored in run.token_hash (InsertRunWithID) and what internal/api's
// authRun middleware compares an incoming request's bearer token against.
func runToken(secret string, runID uuid.UUID) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(runID.String()))

	return hex.EncodeToString(mac.Sum(nil))
}

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))

	return sum[:]
}

// RunLaunch is the run.launch outbox.Handler: it ensures a run row exists
// for the task and asks the daemon to create and start its container.
//
// Idempotent on the outbox message's Key (internal/outbox.Handler's
// contract): run_active_per_task_uk means a second INSERT for the same task
// while a run is still PENDING/STARTING/RUNNING is structurally impossible,
// so a redelivery is detected by GetActiveRunForTask finding the row the
// first attempt already created; the daemon call is then simply repeated
// against the same, deterministically re-derived token — internal/podman's
// Create is itself idempotent by container name, so a daemon call that
// already succeeded once is a cheap no-op on retry.
func (h *Handlers) RunLaunch(ctx context.Context, m outbox.Message) error {
	// Re-checked here, not just at POST /v1/tasks/{id}/start (internal/api's
	// own startTask check): a run.launch row enqueued a moment before an
	// admin hit the kill switch must not start a container just because it
	// beat the pause into the queue. A plain (non-ErrPoison) error retries
	// with backoff — this stays queued and launches once resumed, per
	// development-plan.md §4's kill switch, rather than being dropped.
	if err := h.Store.CheckPause(ctx, "global"); err != nil {
		return fmt.Errorf("run.launch: %w", err)
	}

	var p effectPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		return fmt.Errorf("%w: run.launch: invalid payload: %v", outbox.ErrPoison, err)
	}

	taskID, err := uuid.Parse(p.TaskID)
	if err != nil {
		return fmt.Errorf("%w: run.launch: invalid task_id %q: %v", outbox.ErrPoison, p.TaskID, err)
	}

	q := h.Store.Q()

	// M5: parent_run_id/role are a property of the TASK (set by
	// Store.ApplySpawn when this task was itself spawned by spawn_worker),
	// not something run.launch's own payload carries — every run.launch
	// call for this task (a first launch, a retry, a resume) must derive
	// them the same way, so they're read once here, before the run row
	// itself exists, and threaded into both InsertRunWithID branches below.
	launchCtx, err := q.GetLaunchContext(ctx, taskID)
	if err != nil {
		return fmt.Errorf("run.launch: loading launch context for task %s: %w", taskID, err)
	}

	// M6: the project's compiled manifest (0006_m6.up.sql's project.manifest
	// column, already schema/cross-field validated at registration —
	// internal/api's createProject/updateProjectManifest). Agents is empty
	// for a project registered before M6 or one whose manifest still sits
	// at the '{}' default — every resolution below falls back to
	// h.DefaultRole/h.DefaultModel in that case, same fallback role as
	// api.Server.Manifest/BudgetCaps/FanoutCaps.
	var projectManifest manifest.Manifest
	if err := json.Unmarshal(launchCtx.ProjectManifest, &projectManifest); err != nil {
		return fmt.Errorf("%w: run.launch: task %s's project manifest is not valid JSON: %v", outbox.ErrPoison, taskID, err)
	}

	// Role precedence: an explicit task.role (set by Store.ApplySpawn's
	// spawn_worker role argument) always wins; otherwise a manifest naming
	// exactly one agent has an unambiguous default; otherwise
	// h.DefaultRole, same M2 placeholder as before M6.
	role := h.DefaultRole
	if launchCtx.Role != nil {
		role = *launchCtx.Role
	} else if len(projectManifest.Agents) == 1 {
		for only := range projectManifest.Agents {
			role = only
		}
	}

	agent, hasManifestAgent := projectManifest.Agents[role]

	model := h.DefaultModel
	if hasManifestAgent && agent.Model != "" {
		model = agent.Model
	}

	// promptVersion is an audit column only (internal/domain/prompts.Get
	// resolves it at launch time, not again later) — recorded via
	// SetRunPromptVersion once the run row exists, below.
	var promptVersion, promptText string

	if hasManifestAgent && agent.Prompt != "" {
		text, err := prompts.Get(agent.Prompt)
		if err != nil {
			// manifest.Parse already validates prompt: against
			// prompts.Names() at registration time — reaching here means
			// the prompt library changed out from under an already-stored
			// manifest, not a bad request; ErrPoison would just wedge the
			// task forever, so surface it as a plain retryable error
			// instead in case a redeploy fixes it.
			return fmt.Errorf("run.launch: task %s: resolving prompt %q: %w", taskID, agent.Prompt, err)
		}

		promptVersion = agent.Prompt
		promptText = text
	}

	var patch string

	if hasManifestAgent {
		patchBytes, err := projectManifest.Patch(role)
		if err != nil {
			return fmt.Errorf("run.launch: task %s: compiling manifest patch for role %s: %w", taskID, role, err)
		}

		patch = string(patchBytes)
	}

	// Resume detection (M3): TrAnswered's own run.launch effect carries the
	// JUST-EXITED run's id in RunID — see Store.ApplyAnswer's doc comment.
	// That run already left PENDING/STARTING/RUNNING (either the M3 exit
	// path's ApplyRunTransition, or a crash), so GetActiveRunForTask below
	// finds nothing for it; resolving it here is how this handler tells
	// "resume" from "first launch" without a payload flag of its own.
	var resumeSessionID, answer string

	if p.RunID != "" {
		priorRunID, err := uuid.Parse(p.RunID)
		if err != nil {
			return fmt.Errorf("%w: run.launch: invalid run_id %q: %v", outbox.ErrPoison, p.RunID, err)
		}

		priorRun, err := q.GetRunByID(ctx, priorRunID)
		if err != nil {
			return fmt.Errorf("run.launch: loading prior run %s: %w", priorRunID, err)
		}

		if priorRun.DshSessionID != nil {
			resumeSessionID = *priorRun.DshSessionID
		}

		if p.QuestionID != "" {
			questionID, err := uuid.Parse(p.QuestionID)
			if err != nil {
				return fmt.Errorf("%w: run.launch: invalid question_id %q: %v", outbox.ErrPoison, p.QuestionID, err)
			}

			question, err := q.GetQuestionByID(ctx, questionID)
			if err != nil {
				return fmt.Errorf("run.launch: loading question %s: %w", questionID, err)
			}

			if question.Answer != nil {
				answer = *question.Answer
			}
		}
	}

	run, err := q.GetActiveRunForTask(ctx, taskID)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		runID := uuid.New()
		token := runToken(h.RunTokenSecret, runID)

		run, err = q.InsertRunWithID(ctx, db.InsertRunWithIDParams{
			ID:          runID,
			TaskID:      taskID,
			ParentRunID: launchCtx.ParentRunID,
			Role:        role,
			Model:       model,
			State:       "PENDING",
			TokenHash:   tokenHash(token),
		})
		if err != nil {
			return fmt.Errorf("run.launch: inserting run for task %s: %w", taskID, err)
		}
	case err != nil:
		return fmt.Errorf("run.launch: loading active run for task %s: %w", taskID, err)
	}

	if promptVersion != "" {
		if err := q.SetRunPromptVersion(ctx, db.SetRunPromptVersionParams{ID: run.ID, PromptVersion: &promptVersion}); err != nil {
			return fmt.Errorf("run.launch: recording prompt_version for run %s: %w", run.ID, err)
		}
	}

	var acceptanceCriteria []string
	if err := json.Unmarshal(launchCtx.AcceptanceCriteria, &acceptanceCriteria); err != nil {
		return fmt.Errorf("%w: run.launch: task %s has malformed acceptance_criteria: %v", outbox.ErrPoison, taskID, err)
	}

	task := taskPrompt(launchCtx.Title, launchCtx.Intent, acceptanceCriteria)
	if promptText != "" {
		task = promptText + "\n\n" + task
	}

	if err := h.Daemon.Launch(ctx, LaunchRequest{
		RunID:           run.ID.String(),
		TaskID:          taskID.String(),
		Token:           runToken(h.RunTokenSecret, run.ID),
		Task:            task,
		RepoURL:         launchCtx.RepoUrl,
		Role:            run.Role,
		ResumeSessionID: resumeSessionID,
		Answer:          answer,
		ProjectSlug:     launchCtx.ProjectSlug,
		Patch:           patch,
	}); err != nil {
		return fmt.Errorf("run.launch: daemon launch for run %s: %w", run.ID, err)
	}

	return nil
}

// RunKill is the run.kill outbox.Handler: it asks the daemon to remove the
// task's active run's container, if any. A task with no active run (the
// container already exited on its own before the kill was dispatched) is
// success, not an error — nothing left to kill.
func (h *Handlers) RunKill(ctx context.Context, m outbox.Message) error {
	taskID, err := parseTaskID(m.Payload, "run.kill")
	if err != nil {
		return err
	}

	run, err := h.Store.Q().GetActiveRunForTask(ctx, taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("run.kill: loading active run for task %s: %w", taskID, err)
	}

	if err := h.Daemon.Kill(ctx, run.ID.String()); err != nil {
		return fmt.Errorf("run.kill: daemon kill for run %s: %w", run.ID, err)
	}

	return nil
}

func parseTaskID(payload []byte, topic string) (uuid.UUID, error) {
	var p effectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: %s: invalid payload: %v", outbox.ErrPoison, topic, err)
	}

	taskID, err := uuid.Parse(p.TaskID)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: %s: invalid task_id %q: %v", outbox.ErrPoison, topic, p.TaskID, err)
	}

	return taskID, nil
}

// taskPrompt builds the headless agent's TASK prompt from a task's title,
// intent, and acceptance criteria — plain concatenation, not a template
// engine, since M6's prompt library (development-plan.md §5) is what
// eventually owns real prompt structure; this is a placeholder correct
// enough to drive a real dsh session for M1/M2.
func taskPrompt(title, intent string, acceptanceCriteria []string) string {
	var b strings.Builder

	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString(intent)

	if len(acceptanceCriteria) > 0 {
		b.WriteString("\n\nAcceptance criteria:\n")

		for _, c := range acceptanceCriteria {
			b.WriteString("- ")
			b.WriteString(c)
			b.WriteString("\n")
		}
	}

	return b.String()
}
