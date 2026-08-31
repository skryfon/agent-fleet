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
			from, tr := from, tr
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

func TestEffectSpecRenderKey(t *testing.T) {
	spec := domain.EffectSpec{Topic: "run.launch", KeyTemplate: "launch:{{run_id}}"}
	tc := domain.TransitionContext{RunID: "abc-123"}

	got := spec.RenderKey(tc)
	if want := "launch:abc-123"; got != want {
		t.Fatalf("RenderKey() = %q; want %q", got, want)
	}
}

func TestEffectSpecRenderKeyIsDeterministic(t *testing.T) {
	// Same input, same output, forever — this is what makes a retried
	// transition's outbox key collide with its own earlier attempt instead
	// of drifting.
	spec := domain.EffectSpec{Topic: "run.kill", KeyTemplate: "kill:{{run_id}}"}
	tc := domain.TransitionContext{RunID: "run-9"}

	a := spec.RenderKey(tc)
	b := spec.RenderKey(tc)

	if a != b {
		t.Fatalf("RenderKey is not deterministic: %q != %q", a, b)
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
			from, tr := from, tr
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
