// Package questions implements the M3 timeout ladder (development-plan.md
// §6: "nudge at 4h, escalate at 24h, park the task at 72h. Timeouts never
// auto-answer"). Not internal/reconcile: that package is referenced
// elsewhere as a planned P8 catch-all (stale-run reap/reconciliation) that
// doesn't exist yet — this is a narrow, real M3 job, not a placeholder for
// it. Move here into internal/reconcile if/when P8 actually lands and wants
// to own every periodic sweep in one place.
package questions

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/domain"
	"agentfleet/internal/redact"
	"agentfleet/internal/store"
	db "agentfleet/internal/store/gen"
	"agentfleet/internal/zulip"
)

// Notifier is the subset of internal/zulip.Client the sweeper needs — kept
// narrow so a fake can stand in for tests, matching internal/outbox.Store's
// own convention. *zulip.Client satisfies this as-is.
type Notifier interface {
	Notify(ctx context.Context, req zulip.NotifyRequest) error
}

// Config tunes the timeout ladder's own thresholds — every field has a
// documented default via NewSweeper, matching internal/outbox.Relay's
// Config/DefaultConfig convention.
type Config struct {
	Nudge    time.Duration
	Escalate time.Duration
	Park     time.Duration
	Tick     time.Duration
}

// DefaultConfig is development-plan.md §6's own literal ladder: "nudge at
// 4h, escalate at 24h, park the task at 72h."
func DefaultConfig() Config {
	return Config{
		Nudge:    4 * time.Hour,
		Escalate: 24 * time.Hour,
		Park:     72 * time.Hour,
		Tick:     5 * time.Minute,
	}
}

// Sweeper periodically scans OPEN questions and fires the next unfired rung
// of the timeout ladder for each one overdue.
type Sweeper struct {
	Store  *store.Store
	Redact *redact.Redactor
	Zulip  Notifier
	Log    *slog.Logger
	Config Config
}

func (s *Sweeper) config() Config {
	def := DefaultConfig()
	cfg := s.Config

	if cfg.Nudge <= 0 {
		cfg.Nudge = def.Nudge
	}

	if cfg.Escalate <= 0 {
		cfg.Escalate = def.Escalate
	}

	if cfg.Park <= 0 {
		cfg.Park = def.Park
	}

	if cfg.Tick <= 0 {
		cfg.Tick = def.Tick
	}

	return cfg
}

// Run ticks until ctx is cancelled, sweeping once per tick. A failed sweep
// is logged, not fatal — the next tick tries again.
func (s *Sweeper) Run(ctx context.Context) error {
	cfg := s.config()
	ticker := time.NewTicker(cfg.Tick)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.SweepOnce(ctx); err != nil {
				s.Log.ErrorContext(ctx, "questions: sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce runs a single sweep pass immediately, outside Run's ticker —
// exported for tests (a real, backdated asked_at + time.Now() rather than
// an injected clock) and for a future manual/ops trigger.
func (s *Sweeper) SweepOnce(ctx context.Context) error {
	return s.sweep(ctx, s.config(), time.Now())
}

func (s *Sweeper) sweep(ctx context.Context, cfg Config, now time.Time) error {
	overdue, err := s.Store.Q().ListOverdueQuestions(ctx, pgtype.Timestamptz{Time: now.Add(-cfg.Nudge), Valid: true})
	if err != nil {
		return err
	}

	for _, q := range overdue {
		age := now.Sub(q.AskedAt.Time)

		switch {
		case age >= cfg.Park:
			s.park(ctx, q)
		case age >= cfg.Escalate && q.EscalatedAt.Valid == false: //nolint:staticcheck // explicit for symmetry with the nudge branch below
			s.rung(ctx, q, "escalated", ":rotating_light: **escalated** — still unanswered after 24h:\n\n"+q.Body, s.markEscalated)
		case age >= cfg.Nudge && !q.NudgedAt.Valid:
			s.rung(ctx, q, "nudged", ":bell: **reminder** — still waiting on an answer:\n\n"+q.Body, s.markNudged)
		}
	}

	return nil
}

// rung posts one timeout-ladder notification and marks it fired, so the
// next sweep doesn't repeat it. A post failure is logged and left for the
// next tick to retry (the mark only happens on success) — at-least-once,
// never zero, matching the same tolerance internal/zulip.Handlers.Notify
// documents for review/failed notifications.
func (s *Sweeper) rung(ctx context.Context, q db.Question, label, content string, mark func(context.Context, db.Question) error) {
	topic, err := s.topicFor(ctx, q)
	if err != nil {
		s.Log.ErrorContext(ctx, "questions: resolving topic failed", "question_id", q.ID, "error", err)

		return
	}

	if err := s.Zulip.Notify(ctx, zulip.NotifyRequest{Topic: topic, Content: content}); err != nil {
		s.Log.ErrorContext(ctx, "questions: "+label+" notify failed", "question_id", q.ID, "error", err)

		return
	}

	if err := mark(ctx, q); err != nil {
		s.Log.ErrorContext(ctx, "questions: marking "+label+" failed", "question_id", q.ID, "error", err)
	}
}

func (s *Sweeper) markNudged(ctx context.Context, q db.Question) error {
	_, err := s.Store.Q().MarkQuestionNudged(ctx, q.ID)

	return err
}

func (s *Sweeper) markEscalated(ctx context.Context, q db.Question) error {
	_, err := s.Store.Q().MarkQuestionEscalated(ctx, q.ID)

	return err
}

// park is the 72h terminal rung: times out the question and parks its task.
// The two writes are not one transaction (park is a low-frequency,
// human-visible administrative action, not a hot path) — a crash between
// them leaves the question TIMED_OUT with the task still BLOCKED_ON_HUMAN,
// which the NEXT sweep's ListOverdueQuestions no longer selects (state is
// no longer OPEN) but is otherwise a harmless, human-visible inconsistency
// an operator can resolve by hand.
// ponytail: documented ceiling above; fold into one WithTx if this ever
// needs to be exactly-once.
func (s *Sweeper) park(ctx context.Context, q db.Question) {
	if _, err := s.Store.Q().TimeoutQuestion(ctx, q.ID); err != nil {
		s.Log.ErrorContext(ctx, "questions: timing out question failed", "question_id", q.ID, "error", err)

		return
	}

	if _, err := s.Store.ApplyTaskTransition(ctx, s.Redact, store.TransitionRequest{
		TaskID: q.TaskID, RunID: &q.RunID, Trigger: domain.TrPark, Actor: "questions-sweeper",
	}); err != nil {
		s.Log.ErrorContext(ctx, "questions: parking task failed", "task_id", q.TaskID, "question_id", q.ID, "error", err)
	}
}

func (s *Sweeper) topicFor(ctx context.Context, q db.Question) (string, error) {
	task, err := s.Store.Q().GetTaskByID(ctx, q.TaskID)
	if err != nil {
		return "", err
	}

	feature, err := s.Store.Q().GetFeatureByID(ctx, task.FeatureID)
	if err != nil {
		return "", err
	}

	if feature.ZulipTopic != nil && *feature.ZulipTopic != "" {
		return *feature.ZulipTopic, nil
	}

	return feature.Slug, nil
}
