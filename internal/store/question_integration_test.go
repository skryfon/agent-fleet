//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"agentfleet/internal/redact"
	"agentfleet/internal/store"
)

// TestApplyAskWritesQuestionTransitionAndOutbox is Phase 1's own "done when"
// (see the M3 plan): asking a question through the store leaves the task
// BLOCKED_ON_HUMAN with a zulip.question outbox row referencing it.
func TestApplyAskWritesQuestionTransitionAndOutbox(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	taskID, runID := seedTaskAndRun(t, s)
	r := redact.New(nil, nil)

	result, err := s.ApplyAsk(ctx, r, store.AskRequest{
		RunID: runID, TaskID: taskID,
		Kind: "free_text", Body: "which branch should this target?",
		Actor: "run:" + runID.String(),
	})
	if err != nil {
		t.Fatalf("ApplyAsk: %v", err)
	}

	if result.Task.To != "BLOCKED_ON_HUMAN" {
		t.Fatalf("task state = %s, want BLOCKED_ON_HUMAN", result.Task.To)
	}

	if result.Question.State != "OPEN" {
		t.Fatalf("question state = %s, want OPEN", result.Question.State)
	}

	if len(result.Task.OutboxIDs) != 1 {
		t.Fatalf("outbox rows enqueued = %d, want 1 (zulip.question)", len(result.Task.OutboxIDs))
	}

	task, err := s.Q().GetTaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}

	if task.State != "BLOCKED_ON_HUMAN" {
		t.Fatalf("persisted task state = %s, want BLOCKED_ON_HUMAN", task.State)
	}
}

// TestApplyAskEnforcesOneOpenPerFeature is question_one_open_per_feature_uk
// exercised through the Go layer: a second ask against the same feature
// while the first is still OPEN must fail with ErrQuestionAlreadyOpen, not
// silently succeed or 500.
func TestApplyAskEnforcesOneOpenPerFeature(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	taskID, runID := seedTaskAndRun(t, s)
	r := redact.New(nil, nil)

	if _, err := s.ApplyAsk(ctx, r, store.AskRequest{
		RunID: runID, TaskID: taskID, Kind: "free_text", Body: "first question", Actor: "test",
	}); err != nil {
		t.Fatalf("first ApplyAsk: %v", err)
	}

	// The task is now BLOCKED_ON_HUMAN, so a second ask would also fail
	// NextTask's own (RUNNING, asked) lookup — seed a second RUNNING task on
	// the SAME feature to isolate the constraint this test actually targets.
	task, err := s.Q().GetTaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTaskByID: %v", err)
	}

	task2, run2 := seedTaskOnFeature(t, s, task.FeatureID)

	_, err = s.ApplyAsk(ctx, r, store.AskRequest{
		RunID: run2, TaskID: task2, Kind: "free_text", Body: "second question", Actor: "test",
	})
	if !errors.Is(err, store.ErrQuestionAlreadyOpen) {
		t.Fatalf("second ApplyAsk on the same feature: got %v, want ErrQuestionAlreadyOpen", err)
	}
}

// TestApplyAnswerUnblocksTaskAndEnqueuesResume is the TrAnswered half of the
// round trip: answering the open question returns the task to RUNNING and
// enqueues run.launch/resume.
func TestApplyAnswerUnblocksTaskAndEnqueuesResume(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	taskID, runID := seedTaskAndRun(t, s)
	r := redact.New(nil, nil)

	asked, err := s.ApplyAsk(ctx, r, store.AskRequest{
		RunID: runID, TaskID: taskID, Kind: "confirm", Body: "proceed?", Actor: "test",
	})
	if err != nil {
		t.Fatalf("ApplyAsk: %v", err)
	}

	answered, err := s.ApplyAnswer(ctx, r, store.AnswerRequest{
		QuestionID: asked.Question.ID, Answer: "yes", AnsweredBy: "architect", Actor: "test",
	})
	if err != nil {
		t.Fatalf("ApplyAnswer: %v", err)
	}

	if answered.Task.To != "RUNNING" {
		t.Fatalf("task state = %s, want RUNNING", answered.Task.To)
	}

	if answered.Question.State != "ANSWERED" {
		t.Fatalf("question state = %s, want ANSWERED", answered.Question.State)
	}

	if len(answered.Task.OutboxIDs) != 1 {
		t.Fatalf("outbox rows enqueued = %d, want 1 (run.launch/resume)", len(answered.Task.OutboxIDs))
	}

	// Answering the same question again must fail, not silently re-fire the
	// transition — a stale Zulip reply or a bridge retry must not double-
	// launch a run.
	if _, err := s.ApplyAnswer(ctx, r, store.AnswerRequest{
		QuestionID: asked.Question.ID, Answer: "yes again", AnsweredBy: "architect", Actor: "test",
	}); !errors.Is(err, store.ErrQuestionNotOpen) {
		t.Fatalf("re-answering an already-answered question: got %v, want ErrQuestionNotOpen", err)
	}
}
