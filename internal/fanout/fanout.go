// Package fanout enforces spawn_worker's depth and fan-out limits
// (development-plan.md §5/§7 M5). Pure evaluation, no IO, no clock — same
// shape as internal/budget and internal/policy: internal/api's spawn_worker
// handler is what reads the current depth/children/subtree counts from
// Postgres and reacts to a denial.
package fanout

import "fmt"

// Caps is spawn_worker's configured ceiling. A zero value for any field
// means "uncapped" for that dimension, matching internal/budget.Caps'
// documented convention — a deployment with no FanoutCaps configured must
// not spuriously deny every spawn.
type Caps struct {
	// MaxDepth caps how many spawn_worker hops a child may be from the
	// feature's own root task (task.depth). The root task itself is depth 0.
	MaxDepth int
	// MaxChildrenPerRun caps how many active (non-terminal) child tasks a
	// single run may have spawned at once — the direct fan-out width.
	MaxChildrenPerRun int
	// MaxActiveSubtree caps the total number of active tasks anywhere under
	// the feature's root task, direct or transitive — the aggregate
	// fan-out this run's whole lineage may keep alive at once.
	MaxActiveSubtree int
}

// Decision is Check's verdict. Reason is non-empty iff !Allow — it is
// written verbatim into the policy_violation-shaped event a denied
// spawn_worker call records (internal/api mirrors internal/policy.Decision's
// convention here), so it must never contain a secret; spawn_worker's own
// arguments are not expected to carry one in the first place
// (development-plan.md §8).
type Decision struct {
	Allow  bool
	Reason string
	// Rule names which check fired: "max_depth", "max_children", or
	// "max_subtree" — for the event payload and a metrics label, same
	// convention as internal/policy.Decision.Rule.
	Rule string
}

func allow() Decision { return Decision{Allow: true} }

func deny(rule, reason string) Decision {
	return Decision{Allow: false, Rule: rule, Reason: reason}
}

// Check evaluates one spawn_worker call. childDepth is the depth the NEW
// child task would have (parent's task.depth + 1); siblings is how many
// active children the spawning run has already created; activeSubtree is
// how many active tasks already exist anywhere under the feature's root
// task, INCLUDING the one this call would add if allowed (callers count the
// prospective total, not the count before it, so MaxActiveSubtree reads as
// "at most N active tasks at once," not "at most N-1").
//
// Checked in the order depth, children, subtree — an arbitrary but fixed
// order so Check is deterministic when more than one dimension is over,
// matching internal/budget.Check's own convention.
func Check(childDepth, siblings, activeSubtree int, caps Caps) Decision {
	if caps.MaxDepth > 0 && childDepth > caps.MaxDepth {
		return deny("max_depth", fmt.Sprintf("spawn would reach depth %d, exceeding the cap of %d", childDepth, caps.MaxDepth))
	}

	if caps.MaxChildrenPerRun > 0 && siblings >= caps.MaxChildrenPerRun {
		return deny("max_children", fmt.Sprintf("this run has already spawned %d active children, at the cap of %d", siblings, caps.MaxChildrenPerRun))
	}

	if caps.MaxActiveSubtree > 0 && activeSubtree > caps.MaxActiveSubtree {
		return deny("max_subtree", fmt.Sprintf("the feature's active subtree would reach %d tasks, exceeding the cap of %d", activeSubtree, caps.MaxActiveSubtree))
	}

	return allow()
}
