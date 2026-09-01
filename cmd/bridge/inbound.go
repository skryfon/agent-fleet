package main

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// eventTypes is what this bridge registers for — message (a topic reply,
// development-plan.md §6: "topic reply for free_text") and reaction (an
// emoji answer, "Emoji reaction for choice/confirm").
var eventTypes = []string{"message", "reaction"}

// answerEscapeHatch matches the "/answer <id> <text>" fallback
// (development-plan.md §6) — useful when a reply lands in the wrong topic
// or a question's addressee reads it out of order.
var answerEscapeHatch = regexp.MustCompile(`^/answer\s+(\S+)\s+(.*)$`)

const inboundRetryBackoff = 5 * time.Second

// runInbound drives the register -> long-poll -> re-register-on-expiry loop
// (deploy/zulip/README.md §3b) until ctx is cancelled.
func (d *daemon) runInbound(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := d.inboundSession(ctx); err != nil {
			d.log.Error("bridge: inbound session ended, retrying", "error", err)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(inboundRetryBackoff):
			}
		}
	}

	return nil
}

// inboundSession registers one event queue and polls it until it errors
// (including expiry) or ctx is cancelled.
func (d *daemon) inboundSession(ctx context.Context) error {
	reg, err := d.zulip.RegisterQueue(ctx, d.cfg.stream, eventTypes)
	if err != nil {
		return err
	}

	lastEventID := reg.LastEventID

	for {
		if ctx.Err() != nil {
			return nil
		}

		events, newLastEventID, err := d.zulip.PollEvents(ctx, reg.QueueID, lastEventID)
		if err != nil {
			if errors.Is(err, errBadEventQueue) {
				// Not this session's problem to recover from — the caller's
				// loop re-registers a fresh queue.
				return err
			}

			return err
		}

		lastEventID = newLastEventID

		for _, ev := range events {
			d.handleEvent(ctx, ev)
		}
	}
}

func (d *daemon) handleEvent(ctx context.Context, ev zulipEvent) {
	switch ev.Type {
	case "message":
		d.handleMessage(ctx, ev)
	case "reaction":
		d.handleReaction(ctx, ev)
	}
}

func (d *daemon) handleMessage(ctx context.Context, ev zulipEvent) {
	if ev.Message == nil {
		return
	}

	content := strings.TrimSpace(ev.Message.Content)
	sender := strconv.FormatInt(ev.Message.SenderID, 10)

	if m := answerEscapeHatch.FindStringSubmatch(content); m != nil {
		d.resolveAndAnswer(ctx, sender, "", m[1], m[2])

		return
	}

	d.resolveAndAnswer(ctx, sender, ev.Message.Subject, "", content)
}

// emojiAnswers maps a handful of common confirm/choice reactions to a plain-
// text answer — development-plan.md §6's "Emoji reaction for
// choice/confirm" doesn't specify a vocabulary beyond thumbs-up/down for
// confirm, so this is the documented minimal set; anything else falls back
// to the literal emoji name, still a meaningful answer for a choice
// question whose options happen to be emoji names.
var emojiAnswers = map[string]string{
	"thumbs_up":   "yes",
	"+1":          "yes",
	"thumbs_down": "no",
	"-1":          "no",
}

func (d *daemon) handleReaction(ctx context.Context, ev zulipEvent) {
	if ev.ReactionOp != "add" {
		return
	}

	topic, err := d.zulip.GetMessageTopic(ctx, ev.MessageID)
	if err != nil {
		d.log.Error("bridge: resolving reaction's message topic failed", "message_id", ev.MessageID, "error", err)

		return
	}

	answer := ev.EmojiName
	if mapped, ok := emojiAnswers[ev.EmojiName]; ok {
		answer = mapped
	}

	d.resolveAndAnswer(ctx, strconv.FormatInt(ev.UserID, 10), topic, "", answer)
}

// resolveAndAnswer is the shared tail of both the message and reaction
// paths: verify the sender is a known identity (log-and-ignore if not, per
// §6), resolve the question (either the explicit escape-hatch id, or the
// one open question for topic), then answer it. A failure at any step is
// logged, never surfaced to Zulip — there is no inbound HTTP request to
// fail back to.
func (d *daemon) resolveAndAnswer(ctx context.Context, senderZulipID, topic, explicitQuestionID, answerText string) {
	if answerText == "" {
		return
	}

	id, err := d.controlPlane.GetIdentityByZulip(ctx, senderZulipID)
	if err != nil {
		if _, ok := errors.AsType[errNotFound](err); ok {
			d.log.Info("bridge: ignoring reply from unmapped sender", "zulip_user_id", senderZulipID)

			return
		}

		d.log.Error("bridge: resolving identity failed", "zulip_user_id", senderZulipID, "error", err)

		return
	}

	questionID := explicitQuestionID
	if questionID == "" {
		q, err := d.controlPlane.GetOpenQuestionByTopic(ctx, topic)
		if err != nil {
			if _, ok := errors.AsType[errNotFound](err); ok {
				d.log.Info("bridge: no open question for topic, ignoring reply", "topic", topic)

				return
			}

			d.log.Error("bridge: resolving open question failed", "topic", topic, "error", err)

			return
		}

		questionID = q.ID
	}

	if err := d.controlPlane.AnswerQuestion(ctx, questionID, answerText, id.DisplayName); err != nil {
		d.log.Error("bridge: answering question failed", "question_id", questionID, "error", err)
	}
}
