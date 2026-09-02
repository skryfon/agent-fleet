// M4 safety rails that aren't shaped like a task/run state transition:
// usage accounting + budget breach (development-plan.md §4/§6) and the
// global kill switch (§4: "POST /v1/admin/pause"). Kept separate from
// transition.go/event.go since neither touches domain.NextTask/NextRun.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"agentfleet/internal/budget"
	"agentfleet/internal/domain"
	"agentfleet/internal/redact"
	db "agentfleet/internal/store/gen"
)

// UsageDelta is one POST /v1/runs/{id}/usage report — always a delta since
// the run's last report, never a running total (af-budget has no reason to
// track what it already reported).
type UsageDelta struct {
	TokensIn  int64
	TokensOut int64
	CostUSD   float64
	Minutes   int
	// Questions is 0 for every caller except ApplyAsk (M5's question-cap
	// enforcement, development-plan.md §6: "Cap questions per run (3) and
	// per feature (10)") — af-budget's own usage reports never touch it.
	Questions int
}

// UsageResult reports both scopes' post-increment spend and whichever
// breached first (run checked before feature — an over-budget run is the
// more specific, more actionable signal).
type UsageResult struct {
	RunSpent     budget.Spent
	FeatureSpent budget.Spent
	Breach       *budget.Breach
	// BreachScope is "run" or "feature" — which scope's budget row
	// RecordUsage stamped breached_at on, empty when Breach is nil.
	BreachScope string
}

// RecordUsage accumulates delta onto the run row and both budget scopes
// (run, feature), seeding each scope's budget row from caps on first use
// (UpsertBudgetCaps — see that query's own doc comment for why: the
// manifest compiler that would otherwise own these caps is M6 scope). Not
// itself a task-state transition — internal/api's usage handler is what
// decides to fire TrCancel through ApplyTaskTransition when Breach != nil,
// same separation ApplyAsk/dispatchTool keep between "record the fact" and
// "react to it."
func (s *Store) RecordUsage(ctx context.Context, runID, featureID uuid.UUID, delta UsageDelta, caps budget.Caps) (UsageResult, error) {
	var result UsageResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		if _, err := q.RecordRunUsage(ctx, db.RecordRunUsageParams{
			ID: runID, TokensIn: delta.TokensIn, TokensOut: delta.TokensOut, CostUsd: numericParam(delta.CostUSD),
		}); err != nil {
			return fmt.Errorf("store: recording run usage: %w", err)
		}

		runSpent, err := incrementBudgetScope(ctx, q, "run", runID, delta, caps)
		if err != nil {
			return err
		}

		result.RunSpent = runSpent

		featureSpent, err := incrementBudgetScope(ctx, q, "feature", featureID, delta, caps)
		if err != nil {
			return err
		}

		result.FeatureSpent = featureSpent

		if b := budget.Check(caps, runSpent); b != nil {
			result.Breach = b
			result.BreachScope = "run"

			return q.MarkBudgetBreached(ctx, db.MarkBudgetBreachedParams{ScopeKind: "run", ScopeID: runID})
		}

		if b := budget.Check(caps, featureSpent); b != nil {
			result.Breach = b
			result.BreachScope = "feature"

			return q.MarkBudgetBreached(ctx, db.MarkBudgetBreachedParams{ScopeKind: "feature", ScopeID: featureID})
		}

		return nil
	})

	return result, err
}

func incrementBudgetScope(ctx context.Context, q *db.Queries, scopeKind string, scopeID uuid.UUID, delta UsageDelta, caps budget.Caps) (budget.Spent, error) {
	if _, err := q.UpsertBudgetCaps(ctx, db.UpsertBudgetCapsParams{
		ScopeKind: scopeKind, ScopeID: scopeID,
		UsdCap: numericParam(caps.USD), MinuteCap: int32(caps.Minutes), QuestionCap: int32(caps.Questions), //nolint:gosec // caps are small process config, never user-controlled
	}); err != nil {
		return budget.Spent{}, fmt.Errorf("store: seeding %s budget %s: %w", scopeKind, scopeID, err)
	}

	row, err := q.IncrementBudgetSpent(ctx, db.IncrementBudgetSpentParams{
		ScopeKind: scopeKind, ScopeID: scopeID,
		UsdSpent: numericParam(delta.CostUSD), MinutesSpent: int32(delta.Minutes), QuestionsAsked: int32(delta.Questions), //nolint:gosec // deltas come from af-budget's own token meter or a single ask_human/ask_orchestrator call, bounded well under int32
	})
	if err != nil {
		return budget.Spent{}, fmt.Errorf("store: incrementing %s budget %s: %w", scopeKind, scopeID, err)
	}

	return budget.Spent{USD: float64FromNumeric(row.UsdSpent), Minutes: int(row.MinutesSpent), Questions: int(row.QuestionsAsked)}, nil
}

// ErrPaused is SetPause/CheckPause's shared sentinel — internal/api turns
// it into a 409 (starting a task) or a poison-free retryable outbox error
// (launching a run; see internal/supervisor.RunLaunch).
var ErrPaused = errors.New("store: paused")

// CheckPause returns ErrPaused if the named scope currently has a pause
// row, nil otherwise. M4 only ever checks "global" — see
// 0004_m4.up.sql's comment on why scope is a general text key even though
// per-project enforcement (which would need a task->feature->project join
// at both call sites) is left for a later milestone to wire up; ponytail:
// the table already supports it, only the two call sites don't check it yet.
func (s *Store) CheckPause(ctx context.Context, scope string) error {
	_, err := s.Q().GetPause(ctx, scope)
	if err == nil {
		return ErrPaused
	}

	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}

	return fmt.Errorf("store: checking pause %s: %w", scope, err)
}

// SetPause activates the kill switch for scope, recording an admin_paused
// event against no particular run (RecordEvent needs a run id; a pause is
// process-wide, so this writes straight to the redactor-scrubbed event
// path without one — see insertGlobalEvent below).
func (s *Store) SetPause(ctx context.Context, r *redact.Redactor, scope, actor, reason string) (db.Pause, error) {
	var p db.Pause

	err := s.WithTx(ctx, func(q *db.Queries) error {
		row, err := q.UpsertPause(ctx, db.UpsertPauseParams{Scope: scope, Actor: actor, Reason: &reason})
		if err != nil {
			return fmt.Errorf("store: upserting pause %s: %w", scope, err)
		}

		p = row

		return insertGlobalEvent(ctx, q, r, "admin_paused", map[string]any{"scope": scope, "actor": actor, "reason": reason})
	})

	return p, err
}

// ClearPause deactivates the kill switch for scope.
func (s *Store) ClearPause(ctx context.Context, r *redact.Redactor, scope, actor string) error {
	return s.WithTx(ctx, func(q *db.Queries) error {
		if err := q.DeletePause(ctx, scope); err != nil {
			return fmt.Errorf("store: deleting pause %s: %w", scope, err)
		}

		return insertGlobalEvent(ctx, q, r, "admin_resumed", map[string]any{"scope": scope, "actor": actor})
	})
}

// insertGlobalEvent writes a source='control_plane' event with no run_id/
// task_id — the admin_paused/admin_resumed pair are the only two event
// kinds this codebase writes without one, since a pause is scoped to a
// project or the whole deployment, never a single run. seq is always 0:
// these rows aren't part of any run's or task's ordered event stream (the
// columns that seq disambiguates), just the append-only audit log.
func insertGlobalEvent(ctx context.Context, q *db.Queries, r *redact.Redactor, kind string, payload map[string]any) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store: marshaling %s payload: %w", kind, err)
	}

	redacted, err := r.JSON(payloadJSON)
	if err != nil {
		return fmt.Errorf("store: redacting %s payload: %w", kind, err)
	}

	_, err = q.InsertControlPlaneEvent(ctx, db.InsertControlPlaneEventParams{
		Kind: kind, Actor: domain.ActorControlPlane, Payload: redacted,
	})
	if err != nil {
		return fmt.Errorf("store: inserting %s event: %w", kind, err)
	}

	return nil
}
