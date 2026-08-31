// Package policy is the Go-side tool-dispatch policy engine: a pure
// function (role, tool, args, manifest) -> allow | deny(reason), no side
// effects, golden-tested (.claude/CLAUDE.md).
//
// This is deliberately the THIRD line of defense, not a duplicate of the
// first two:
//  1. The manifest's tool allow-list (compiled by the control plane,
//     development-plan.md §5) — an unregistered tool never reaches the
//     model's schema at all.
//  2. af-policy (runner/packages/af-policy, TS, M1) — a `tools/pre-execute`
//     waterfall gating LOCAL tools that execute inside the runner
//     (read/write/bash in the worktree).
//  3. This package — gates MEDIATED tools that have already crossed the
//     boundary to POST /v1/runs/{id}/tools/{name} (development-plan.md §4:
//     "Mediated tools (spawn, ask, PR creation, anything crossing a
//     boundary) go through the API so the decision is recorded as an
//     event"). Neither of the first two lines runs in the same process as
//     this one, and neither substitutes for it — a runner compromised
//     enough to bypass its own af-policy still has to get through this
//     package's Evaluate before the control plane will act.
package policy

import (
	"encoding/json"
	"slices"
)

// Manifest is the compiled per-project tool policy — one Role per manifest
// role name (development-plan.md §5's `.agentfleet/project.yaml` compiles
// to something shaped like this; the manifest compiler itself is M6 scope,
// out of this package).
type Manifest struct {
	Roles map[string]Role `json:"roles"`
}

// Op names a comparison Rule applies to one argument path's value.
type Op string

const (
	OpEquals Op = "equals"
	OpNotIn  Op = "not_in" // Value is a comma-separated list
	OpIn     Op = "in"     // Value is a comma-separated list
)

// Rule is one argument-level predicate — e.g. {Path: "base", Op: OpNotIn,
// Value: "main,master"} on gh_pr_create denies targeting a protected branch
// even though gh_pr_create itself is allowed.
type Rule struct {
	Path  string `json:"path"`
	Op    Op     `json:"op"`
	Value string `json:"value"`
}

// Role is one manifest role's mediated-tool policy.
type Role struct {
	// MediatedTools is the allow-list: a tool absent from it is denied,
	// full stop — allow-list first (.claude/CLAUDE.md).
	MediatedTools []string `json:"mediated_tools"`
	// Deny is a contextual deny-list that wins even over a tool present in
	// MediatedTools — the manifest may allow gh_pr_create in general while
	// a specific deployment still wants it off for this role.
	Deny []string `json:"deny"`
	// ArgRules are additional per-tool argument predicates, keyed by tool
	// name. All rules for a tool must pass (AND) or the call is denied.
	ArgRules map[string][]Rule `json:"arg_rules"`
}

// hardDenyTools can never be allowed by any manifest, for any role — the
// floor under D3's four-layer merge-prevention story (development-plan.md
// §5: "af-github ... No merge tool exists" — this is what backs that up if
// one somehow got registered anyway). A manifest that lists one of these in
// MediatedTools is a manifest bug, not a grant.
var hardDenyTools = map[string]bool{
	"merge":       true,
	"gh_pr_merge": true,
}

// Request is one mediated tool-dispatch call to evaluate.
type Request struct {
	Role     string
	Tool     string
	Args     json.RawMessage
	Manifest Manifest
}

// Decision is Evaluate's verdict. Reason is non-empty iff !Allow — it is
// written verbatim into the policy_violation event payload
// (internal/store.ApplyTaskTransition's caller composes that event), so it
// must never itself contain a secret; policy Args are not expected to
// carry credentials in the first place (development-plan.md §8: "secrets
// never enter agent context"), but Evaluate does not redact — that is
// internal/redact's job at the event-write choke point, not this package's.
type Decision struct {
	Allow  bool
	Reason string
	// Rule names which check fired, for the event payload and for a metrics
	// label — "hard_deny", "unknown_role", "not_allow_listed", "role_deny",
	// or "arg_rule:<path>".
	Rule string
}

func allow() Decision { return Decision{Allow: true} }

func deny(rule, reason string) Decision {
	return Decision{Allow: false, Rule: rule, Reason: reason}
}

// Evaluate is pure: no clock, no IO, no globals, no network — the same
// Request always produces the same Decision. Malformed Args deny rather
// than allow (fail closed), per the same instinct that makes an unparsable
// af-policy bash command deny in M1's runner-side plugin.
func Evaluate(req Request) Decision {
	if hardDenyTools[req.Tool] {
		return deny("hard_deny", req.Tool+" is never permitted, regardless of manifest")
	}

	role, ok := req.Manifest.Roles[req.Role]
	if !ok {
		return deny("unknown_role", "role "+req.Role+" is not declared in this project's manifest")
	}

	if slices.Contains(role.Deny, req.Tool) {
		return deny("role_deny", req.Tool+" is denied for role "+req.Role)
	}

	if !slices.Contains(role.MediatedTools, req.Tool) {
		return deny("not_allow_listed", req.Tool+" is not in role "+req.Role+"'s mediated_tools allow-list")
	}

	rules, hasRules := role.ArgRules[req.Tool]
	if !hasRules {
		return allow()
	}

	var args map[string]any
	if len(req.Args) > 0 {
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return deny("malformed_args", "arguments are not valid JSON: "+err.Error())
		}
	}

	for _, rule := range rules {
		if d := evaluateRule(rule, args); !d.Allow {
			return d
		}
	}

	return allow()
}

func evaluateRule(rule Rule, args map[string]any) Decision {
	raw, present := args[rule.Path]
	value, isString := raw.(string)

	switch rule.Op {
	case OpEquals:
		if !present || !isString || value != rule.Value {
			return deny("arg_rule:"+rule.Path, rule.Path+" must equal "+rule.Value)
		}
	case OpIn:
		if !present || !isString || !slices.Contains(splitCSV(rule.Value), value) {
			return deny("arg_rule:"+rule.Path, rule.Path+" must be one of "+rule.Value)
		}
	case OpNotIn:
		// Fail closed like OpEquals/OpIn above: an absent or non-string arg
		// is a denial, not a pass. Caught in code review — the original
		// `present && isString && ...` form let a caller evade the rule
		// entirely by simply omitting the argument, which is exactly the
		// package doc's own worked example ({Path: "base", Op: OpNotIn,
		// Value: "main,master"}) — omitting `base` would have bypassed the
		// protected-branch check it exists to enforce.
		if !present || !isString || slices.Contains(splitCSV(rule.Value), value) {
			return deny("arg_rule:"+rule.Path, rule.Path+" must be present and must not be one of "+rule.Value)
		}
	default:
		// An unrecognized Op is a manifest bug — fail closed rather than
		// silently skip the rule it was supposed to enforce.
		return deny("arg_rule:"+rule.Path, "unrecognized rule op "+string(rule.Op))
	}

	return allow()
}

func splitCSV(s string) []string {
	var out []string

	start := 0

	for i := range len(s) {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}

	out = append(out, s[start:])

	return out
}
