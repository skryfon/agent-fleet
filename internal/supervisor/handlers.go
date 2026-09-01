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
	// DefaultRole/DefaultModel are the M2 placeholder role/model for every
	// launched run — development-plan.md §5's model-allocation table
	// becomes real once M6's manifest compiler resolves them per task/role
	// instead of a single process-wide default.
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
	var p effectPayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		return fmt.Errorf("%w: run.launch: invalid payload: %v", outbox.ErrPoison, err)
	}

	taskID, err := uuid.Parse(p.TaskID)
	if err != nil {
		return fmt.Errorf("%w: run.launch: invalid task_id %q: %v", outbox.ErrPoison, p.TaskID, err)
	}

	q := h.Store.Q()

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
			ID:        runID,
			TaskID:    taskID,
			Role:      h.DefaultRole,
			Model:     h.DefaultModel,
			State:     "PENDING",
			TokenHash: tokenHash(token),
		})
		if err != nil {
			return fmt.Errorf("run.launch: inserting run for task %s: %w", taskID, err)
		}
	case err != nil:
		return fmt.Errorf("run.launch: loading active run for task %s: %w", taskID, err)
	}

	launchCtx, err := q.GetLaunchContext(ctx, taskID)
	if err != nil {
		return fmt.Errorf("run.launch: loading launch context for task %s: %w", taskID, err)
	}

	var acceptanceCriteria []string
	if err := json.Unmarshal(launchCtx.AcceptanceCriteria, &acceptanceCriteria); err != nil {
		return fmt.Errorf("%w: run.launch: task %s has malformed acceptance_criteria: %v", outbox.ErrPoison, taskID, err)
	}

	if err := h.Daemon.Launch(ctx, LaunchRequest{
		RunID:           run.ID.String(),
		TaskID:          taskID.String(),
		Token:           runToken(h.RunTokenSecret, run.ID),
		Task:            taskPrompt(launchCtx.Title, launchCtx.Intent, acceptanceCriteria),
		RepoURL:         launchCtx.RepoUrl,
		Role:            run.Role,
		ResumeSessionID: resumeSessionID,
		Answer:          answer,
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
