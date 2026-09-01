package zulip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"agentfleet/internal/outbox"
	db "agentfleet/internal/store/gen"
)

// effectPayload mirrors internal/store/transition.go's unexported
// effectPayload{TaskID, RunID, QuestionID} — the JSON body every
// zulip.question/zulip.review/zulip.failed outbox row carries (same
// convention internal/supervisor/handlers.go's own copy documents).
type effectPayload struct {
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
}

// Store is the subset of *internal/store.Store Handlers needs — kept
// narrow, like internal/supervisor.Store, so a fake can stand in for tests.
type Store interface {
	Q() *db.Queries
}

// Handlers holds the outbox handler(s) this package registers on
// internal/outbox.Relay — currently just Notify, for all three zulip.*
// topics.
type Handlers struct {
	Store  Store
	Client *Client
	// DefaultStream is the Zulip channel every topic lives under —
	// ZULIP_STREAM, since a single team-scale deployment (development-plan.md
	// §11's team size) has no reason yet for per-project channels; add that
	// once a real deployment needs it.
	DefaultStream string
}

// parseEffectPayload mirrors internal/supervisor/handlers.go's
// parseTaskID — a malformed payload is a configuration/code bug, not a
// transient failure, so it poisons immediately rather than retrying forever.
func parseEffectPayload(payload []byte, topic string) (effectPayload, error) {
	var p effectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return effectPayload{}, fmt.Errorf("%w: %s: unmarshaling payload: %v", outbox.ErrPoison, topic, err)
	}

	return p, nil
}

// Notify is the zulip.question/zulip.review/zulip.failed outbox.Handler. It
// resolves the task's feature (and that feature's Zulip topic, or its slug
// as a documented fallback — deploy/zulip/README.md §6), composes a message
// for the reason the topic names, and asks the bridge daemon to post it.
//
// Idempotent on m.Key for the question reason: a redelivered row whose
// question already carries a zulip_message_id is a no-op — Zulip itself has
// no dedupe of its own, so this is the mechanism outbox.Handler's contract
// requires. review/failed have no per-effect row to persist a marker on;
// // ponytail: at-least-once there, a duplicate "ready for review" ping is
// harmless, tighten only if it proves not to be.
func (h *Handlers) Notify(ctx context.Context, m outbox.Message) error {
	p, err := parseEffectPayload(m.Payload, m.Topic)
	if err != nil {
		return err
	}

	taskID, err := uuid.Parse(p.TaskID)
	if err != nil {
		return fmt.Errorf("%w: %s: invalid task_id %q: %v", outbox.ErrPoison, m.Topic, p.TaskID, err)
	}

	q := h.Store.Q()

	task, err := q.GetTaskByID(ctx, taskID)
	if err != nil {
		return fmt.Errorf("zulip: loading task %s: %w", taskID, err)
	}

	feature, err := q.GetFeatureByID(ctx, task.FeatureID)
	if err != nil {
		return fmt.Errorf("zulip: loading feature %s: %w", task.FeatureID, err)
	}

	topicName := feature.Slug
	if feature.ZulipTopic != nil && *feature.ZulipTopic != "" {
		topicName = *feature.ZulipTopic
	}

	switch m.Topic {
	case "zulip.question":
		return h.notifyQuestion(ctx, q, p, task, topicName)
	case "zulip.review":
		return h.Client.Notify(ctx, NotifyRequest{
			Topic:   topicName,
			Content: fmt.Sprintf(":mag: **%s** is ready for review.", task.Title),
		})
	case "zulip.failed":
		return h.Client.Notify(ctx, NotifyRequest{
			Topic:   topicName,
			Content: fmt.Sprintf(":x: **%s** failed after exhausting its retries.", task.Title),
		})
	default:
		// A topic this package didn't register for reaching Notify is a
		// wiring bug in cmd/control-plane/main.go, not a data problem.
		return fmt.Errorf("%w: %s: unrecognized zulip notify reason", outbox.ErrPoison, m.Topic)
	}
}

func (h *Handlers) notifyQuestion(ctx context.Context, q *db.Queries, p effectPayload, task db.Task, topicName string) error {
	questionID, err := uuid.Parse(p.QuestionID)
	if err != nil {
		return fmt.Errorf("%w: zulip.question: invalid question_id %q: %v", outbox.ErrPoison, p.QuestionID, err)
	}

	question, err := q.GetQuestionByID(ctx, questionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: zulip.question: question %s does not exist", outbox.ErrPoison, questionID)
		}

		return fmt.Errorf("zulip: loading question %s: %w", questionID, err)
	}

	if question.ZulipMessageID != nil {
		// Already posted by an earlier attempt at this same outbox row —
		// the idempotency the Handler contract requires.
		return nil
	}

	content := fmt.Sprintf(":raising_hand: **%s** is asking:\n\n%s", task.Title, question.Body)
	if question.Kind == "confirm" {
		content += "\n\n_react with :thumbsup:/:thumbsdown: or reply to answer._"
	}

	if err := h.Client.Notify(ctx, NotifyRequest{Topic: topicName, Content: content}); err != nil {
		return err
	}

	// The bridge doesn't hand back a Zulip message id today (POST /notify's
	// response carries none — see cmd/bridge/daemon.go); persist a
	// non-empty marker so the idempotency check above still fires on
	// redelivery. Revisit once the bridge's response threads the real
	// Zulip message id through, if per-message editing/reactions need it.
	posted := "posted"
	if _, err := q.SetQuestionZulipMessageID(ctx, db.SetQuestionZulipMessageIDParams{
		ID: questionID, ZulipMessageID: &posted,
	}); err != nil {
		return fmt.Errorf("zulip: marking question %s notified: %w", questionID, err)
	}

	return nil
}
