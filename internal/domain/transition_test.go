package domain_test

import (
	"errors"
	"testing"

	"agentfleet/internal/domain"
)

// legalTaskCells is the exhaustive set of (from, trigger) pairs the table in
// transition.go declares legal. Anything NOT in this set, for every
// combination of every state and every trigger, MUST return
// ErrIllegalTransition — that is the "illegal transitions error, never a
// silent no-op" invariant from development-plan.md §3 and docs/adr/0010,
// verified exhaustively rather than by example.
func legalTaskCells() map[[2]string]domain.TaskState {
	legal := make(map[[2]string]domain.TaskState)
	for _, row := range domain.TaskTable() {
		key := [2]string{string(row.From), string(row.Trigger)}
		if _, dup := legal[key]; dup {
			panic("duplicate (From, Trigger) row in TaskTable: " + key[0] + "/" + key[1])
		}

		legal[key] = row.To
	}

	return legal
}

func allTriggers() []domain.Trigger {
	return []domain.Trigger{
		domain.TrIngested, domain.TrStart, domain.TrRunStarted, domain.TrRunExitedOK,
		domain.TrRunExitedErrRetryable, domain.TrRunExitedErrFinal, domain.TrAsked,
		domain.TrAnswered, domain.TrCancel, domain.TrPark, domain.TrApproved,
	}
}

func TestNextTaskGrid(t *testing.T) {
	legal := legalTaskCells()
	tc := domain.TransitionContext{TaskID: "t1", RunID: "r1", RequestedBy: "test"}

	for _, from := range domain.AllTaskStates() {
		for _, tr := range allTriggers() {
			t.Run(string(from)+"/"+string(tr), func(t *testing.T) {
				wantTo, isLegal := legal[[2]string{string(from), string(tr)}]

				outcome, err := domain.NextTask(from, tr, tc)
				if !isLegal {
					if !errors.Is(err, domain.ErrIllegalTransition) {
						t.Fatalf("NextTask(%s, %s) = (%+v, %v); want ErrIllegalTransition", from, tr, outcome, err)
					}

					return
				}

				if err != nil {
					t.Fatalf("NextTask(%s, %s) unexpected error: %v", from, tr, err)
				}

				if outcome.To != wantTo {
					t.Fatalf("NextTask(%s, %s).To = %s; want %s", from, tr, outcome.To, wantTo)
				}

				if outcome.Event.Kind == "" {
					t.Fatalf("NextTask(%s, %s) produced an Outcome with no EventKind — every transition must write an event", from, tr)
				}
			})
		}
	}
}

func TestNextTaskEveryStateReachableAndNoDeadEnd(t *testing.T) {
	legal := legalTaskCells()

	reachable := map[domain.TaskState]bool{domain.TaskCreated: true}
	for _, to := range legal {
		reachable[to] = true
	}

	for _, s := range domain.AllTaskStates() {
		if !reachable[s] {
			t.Errorf("state %s is never reached by any transition in TaskTable()", s)
		}
	}

	terminal := map[domain.TaskState]bool{domain.TaskDone: true, domain.TaskFailed: true, domain.TaskCancelled: true, domain.TaskParked: true}
	hasOutbound := map[domain.TaskState]bool{}

	for key := range legal {
		hasOutbound[domain.TaskState(key[0])] = true
	}

	for _, s := range domain.AllTaskStates() {
		if terminal[s] {
			continue
		}

		if !hasOutbound[s] {
			t.Errorf("non-terminal state %s has no outbound transition in TaskTable() — it would be a dead end", s)
		}
	}
}

// TestEffectsHaveKeyReasonNotEmpty catches the class of bug an earlier
// version of this table actually had: the initial QUEUED->RUNNING launch
// effect was keyed by "launch:{{run_id}}", but no run exists yet at that
// point in the transition (the launch effect is what creates one) — every
// task's first launch rendered the identical literal key forever, and only
// the SECOND task to ever start silently failed to enqueue (ON CONFLICT DO
// NOTHING against the first task's key). Caught by
// internal/outbox.TestTransitionThenRelayDispatch failing, not by a domain
// test — this test exists so the domain layer itself would have caught it:
// every effect must carry a non-empty KeyReason, and internal/store keys it
// using the transition's own always-fresh event id (never a template
// substituted from possibly-absent identifiers).
func TestEffectsHaveKeyReasonNotEmpty(t *testing.T) {
	for _, row := range domain.TaskTable() {
		for _, eff := range row.Effects {
			if eff.KeyReason == "" {
				t.Errorf("%s+%s effect on topic %q has an empty KeyReason", row.From, row.Trigger, eff.Topic)
			}
		}
	}
}

var runTriggers = []domain.Trigger{domain.TrRunStarted, domain.TrRunExitedOK, domain.TrRunFailed, domain.TrCancel}

func TestNextRunGrid(t *testing.T) {
	legal := make(map[[2]string]domain.RunState)
	for _, row := range domain.RunTable() {
		legal[[2]string{string(row.From), string(row.Trigger)}] = row.To
	}

	tc := domain.TransitionContext{RunID: "r1", RequestedBy: "test"}

	for _, from := range domain.AllRunStates() {
		for _, tr := range runTriggers {
			t.Run(string(from)+"/"+string(tr), func(t *testing.T) {
				wantTo, isLegal := legal[[2]string{string(from), string(tr)}]

				outcome, err := domain.NextRun(from, tr, tc)
				if !isLegal {
					if !errors.Is(err, domain.ErrIllegalTransition) {
						t.Fatalf("NextRun(%s, %s) = (%+v, %v); want ErrIllegalTransition", from, tr, outcome, err)
					}

					return
				}

				if err != nil {
					t.Fatalf("NextRun(%s, %s) unexpected error: %v", from, tr, err)
				}

				if outcome.To != wantTo {
					t.Fatalf("NextRun(%s, %s).To = %s; want %s", from, tr, outcome.To, wantTo)
				}
			})
		}
	}
}
