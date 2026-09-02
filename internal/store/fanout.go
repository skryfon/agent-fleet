// M5 orchestration: spawn_worker (a new child task, born already QUEUED and
// immediately started) and subtree cancellation (development-plan.md §5/§7
// M5). Kept separate from transition.go since both operate over MULTIPLE
// task rows in one transaction — ApplySpawn drives two transitions
// (TrIngested then TrStart) on the new child, CancelSubtree drives TrCancel
// across an entire recursive set — unlike every method in transition.go,
// which drives exactly one task's (and, for ApplyRunExit, one run's) own
// transition.
package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"agentfleet/internal/domain"
	"agentfleet/internal/redact"
	db "agentfleet/internal/store/gen"
)

// SpawnRequest is what internal/api's spawn_worker handler hands to
// ApplySpawn. The caller (internal/api/tools.go) has already run
// internal/fanout.Check against the parent task's depth/sibling/subtree
// counts before calling this — ApplySpawn itself does not re-check caps, it
// only performs the insert+transition once a spawn is already decided.
type SpawnRequest struct {
	// ParentTaskID is the SPAWNING RUN's OWN task — the child's feature_id
	// and depth are derived from this task's row, locked for the duration
	// of the transaction so a concurrent spawn from a sibling call can't
	// race the depth/feature read.
	ParentTaskID uuid.UUID
	// ParentRunID is the run whose spawn_worker call this is — becomes the
	// child task's task.parent_run_id (CancelSubtree's own recursive walk
	// key), and the run the worker_spawned event is recorded against.
	ParentRunID        uuid.UUID
	Title, Intent      string
	AcceptanceCriteria []byte // jsonb; nil defaults to '[]'
	// Role, if non-empty, becomes the child task's task.role — internal/
	// supervisor.RunLaunch reads it in place of DefaultRole. Empty leaves
	// task.role NULL (use the process-wide default).
	Role      string
	Actor     string
	DedupeKey *string
}

// SpawnResult reports the newly created child task alongside both
// transitions ApplySpawn drove on it (TrIngested then TrStart).
type SpawnResult struct {
	Child             db.Task
	Ingested, Started TransitionResult
}

// ApplySpawn is spawn_worker's write path: lock the spawning run's own
// task (for its feature_id and depth), insert the child task one depth
// level deeper, drive it CREATED->QUEUED->RUNNING via the ordinary
// TrIngested/TrStart transitions (the same ones POST /v1/features/{id}/
// tasks:ingest and POST /v1/tasks/{id}/start would drive — spawn_worker
// reuses the state machine rather than inventing a parallel one), and
// records a worker_spawned event against the PARENT run so the parent's own
// event stream shows the spawn without a second query. All in one WithTx.
func (s *Store) ApplySpawn(ctx context.Context, r *redact.Redactor, req SpawnRequest) (SpawnResult, error) {
	var result SpawnResult

	acceptanceCriteria := req.AcceptanceCriteria
	if acceptanceCriteria == nil {
		acceptanceCriteria = []byte(`[]`)
	}

	err := s.WithTx(ctx, func(q *db.Queries) error {
		parent, err := q.GetTaskForUpdate(ctx, req.ParentTaskID)
		if err != nil {
			return fmt.Errorf("store: loading parent task %s for update: %w", req.ParentTaskID, err)
		}

		var role *string
		if req.Role != "" {
			role = &req.Role
		}

		child, err := q.InsertChildTask(ctx, db.InsertChildTaskParams{
			FeatureID:          parent.FeatureID,
			Title:              req.Title,
			Intent:             req.Intent,
			AcceptanceCriteria: acceptanceCriteria,
			State:              string(domain.TaskCreated),
			ParentRunID:        uuidParam(req.ParentRunID),
			Depth:              parent.Depth + 1,
			Role:               role,
		})
		if err != nil {
			return fmt.Errorf("store: inserting child task: %w", err)
		}

		ingested, err := applyChildTransition(ctx, q, r, child.ID, domain.TrIngested, req.Actor, nil)
		if err != nil {
			return err
		}

		started, err := applyChildTransition(ctx, q, r, child.ID, domain.TrStart, req.Actor, nil)
		if err != nil {
			return err
		}

		if _, err := insertTransitionEvent(ctx, q, r, uuidParam(req.ParentRunID), uuidParam(req.ParentTaskID), parent.NextEventSeq, domain.PendingEvent{
			Kind: "worker_spawned", Actor: domain.ActorControlPlane,
			Payload: map[string]any{"child_task_id": child.ID.String(), "title": req.Title},
		}, req.DedupeKey); err != nil {
			return err
		}

		result = SpawnResult{Child: child, Ingested: ingested, Started: started}

		return nil
	})

	return result, err
}

// applyChildTransition re-locks taskID (its state may have just changed
// under this same transaction — a fresh GetTaskForUpdate is how it learns
// its CURRENT state rather than trusting a Go-side struct that may already
// be one transition stale) and drives one transition on it, with no run id
// attached to the event or the effect payload (a spawned child's own run
// row does not exist yet at TrIngested, and run.launch's effect payload
// only ever needs TaskID — see effectPayload's own doc comment). ApplySpawn
// calls this twice (TrIngested has no effect, TrStart schedules
// run.launch) rather than duplicating ApplyTaskTransition's body a third
// time in this file.
func applyChildTransition(ctx context.Context, q *db.Queries, r *redact.Redactor, taskID uuid.UUID, tr domain.Trigger, actor string, dedupeKey *string) (TransitionResult, error) {
	task, err := q.GetTaskForUpdate(ctx, taskID)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("store: loading child task %s for update: %w", taskID, err)
	}

	tc := domain.TransitionContext{TaskID: taskID.String(), RequestedBy: actor}
	from := domain.TaskState(task.State)

	outcome, err := domain.NextTask(from, tr, tc)
	if err != nil {
		return TransitionResult{}, err
	}

	if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: taskID, State: string(outcome.To)}); err != nil {
		return TransitionResult{}, fmt.Errorf("store: updating child task state: %w", err)
	}

	ev, err := insertTransitionEvent(ctx, q, r, pgtype.UUID{}, uuidParam(taskID), task.NextEventSeq, outcome.Event, dedupeKey)
	if err != nil {
		return TransitionResult{}, err
	}

	effPayloadJSON, err := json.Marshal(effectPayload{TaskID: taskID.String()})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("store: marshaling effect payload: %w", err)
	}

	outboxIDs, err := enqueueEffects(ctx, q, outcome.Effects, ev.ID, effPayloadJSON)
	if err != nil {
		return TransitionResult{}, err
	}

	return TransitionResult{From: from, To: outcome.To, EventID: ev.ID, OutboxIDs: outboxIDs}, nil
}

// CancelSubtree cancels rootTaskID and every task transitively spawned by
// any run of it (internal/store/queries/task.sql's ListActiveSubtreeTaskIDs
// recursive CTE), each via the ordinary TrCancel transition — so each one's
// own run.kill effect enqueues exactly the way a single-task cancel already
// does. All tasks are locked and cancelled inside ONE transaction: a
// subtree is never left partially cancelled by a crash mid-walk.
//
// A domain.ErrIllegalTransition here (a task the CTE listed as active
// somehow already isn't, by the time this loop reaches it — impossible
// within one transaction today, since the CTE read and every cancel below
// share the same snapshot, but ListActiveSubtreeTaskIDs's exclusion list
// and domain.NextTask's transition table are two independently-maintained
// sources of "terminal") aborts the whole cancel rather than being
// swallowed — a future edit to one without the other must fail loud.
func (s *Store) CancelSubtree(ctx context.Context, r *redact.Redactor, rootTaskID uuid.UUID, actor string) ([]TransitionResult, error) {
	var results []TransitionResult

	err := s.WithTx(ctx, func(q *db.Queries) error {
		ids, err := q.ListActiveSubtreeTaskIDs(ctx, rootTaskID)
		if err != nil {
			return fmt.Errorf("store: listing active subtree for task %s: %w", rootTaskID, err)
		}

		results = make([]TransitionResult, 0, len(ids))

		for _, id := range ids {
			task, err := q.GetTaskForUpdate(ctx, id)
			if err != nil {
				return fmt.Errorf("store: loading task %s for update: %w", id, err)
			}

			tc := domain.TransitionContext{TaskID: id.String(), RequestedBy: actor}
			from := domain.TaskState(task.State)

			outcome, err := domain.NextTask(from, domain.TrCancel, tc)
			if err != nil {
				return fmt.Errorf("store: cancelling subtree task %s: %w", id, err)
			}

			if _, err := q.UpdateTaskState(ctx, db.UpdateTaskStateParams{ID: id, State: string(outcome.To)}); err != nil {
				return fmt.Errorf("store: updating task state: %w", err)
			}

			ev, err := insertTransitionEvent(ctx, q, r, pgtype.UUID{}, uuidParam(id), task.NextEventSeq, outcome.Event, nil)
			if err != nil {
				return err
			}

			effPayloadJSON, err := json.Marshal(effectPayload{TaskID: id.String()})
			if err != nil {
				return fmt.Errorf("store: marshaling effect payload: %w", err)
			}

			outboxIDs, err := enqueueEffects(ctx, q, outcome.Effects, ev.ID, effPayloadJSON)
			if err != nil {
				return err
			}

			results = append(results, TransitionResult{From: from, To: outcome.To, EventID: ev.ID, OutboxIDs: outboxIDs})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return results, nil
}
