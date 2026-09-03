// Package manifest parses, validates, and compiles .agentfleet/project.yaml
// (development-plan.md §5, §7 M6). It is the M6 manifest compiler that
// internal/policy's package doc and cmd/control-plane/main.go's Manifest/
// BudgetCaps/FanoutCaps TODOs point at: internal/api registers a project's
// raw manifest text, this package turns it into a Manifest, and Manifest's
// own methods derive everything downstream (policy.Manifest, budget.Caps,
// fanout.Caps, the compiled dsh --patch overlay) needs — one parse, several
// pure projections, no second source of truth.
//
// Modeled directly on internal/domain/tasksmd: a YAML document validated
// against an embedded JSON Schema, decoded into typed Go structs, with
// EVERY issue reported at once and a real line number per issue.
package manifest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"agentfleet/internal/budget"
	"agentfleet/internal/domain/prompts"
	"agentfleet/internal/fanout"
	"agentfleet/internal/policy"
)

//go:embed schema/manifest.v1.schema.json
var schemaJSON []byte

// schemaURL is this schema's permanent identity, matching internal/domain/
// tasksmd's convention that the embedded $id is already the eventual public
// URL — promoting it to a standalone schema repo is a relocation, not a
// rewrite.
const schemaURL = "https://agentfleet.dev/schemas/manifest/v1"

var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("manifest: embedded schema is not valid JSON: %v", err))
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		panic(fmt.Sprintf("manifest: embedded schema failed to register: %v", err))
	}

	return c.MustCompile(schemaURL)
}

// mediatedTools is every tool name that ever crosses POST
// /v1/runs/{id}/tools/{name} — internal/policy's package doc, §4's mediated
// vs. local tool split. A manifest's `tools:` list may also name LOCAL
// tools (read/write/bash/git — allow-listed to the model by dsh's own tool
// schema, never evaluated by internal/policy at all); Policy() below
// intersects `tools:` with this set precisely so a role's local tools don't
// show up as an accidental policy.Manifest allow-list entry that
// internal/policy.Evaluate would then be asked to gate for a tool it will
// never actually see a dispatch call for.
var mediatedTools = map[string]bool{
	"ask_human":              true,
	"ask_orchestrator":       true,
	"answer_worker":          true,
	"report_to_orchestrator": true,
	"spawn_worker":           true,
	"check_workers":          true,
	"gh_pr_create":           true,
	"pr_opened":              true,
	"report_deviation":       true,
}

// Issue is one problem found while parsing or validating a manifest — same
// shape and same "report every issue, not just the first" contract as
// internal/domain/tasksmd.Issue.
type Issue struct {
	Line    int
	Path    string
	Message string
}

func (i Issue) String() string {
	if i.Line > 0 {
		return fmt.Sprintf("%d: %s: %s", i.Line, i.Path, i.Message)
	}

	return i.Message
}

// ArgRule mirrors internal/policy.Rule (kept as its own type here rather
// than importing policy.Rule directly into the YAML-decoded shape, so this
// package's on-disk schema doesn't silently change if internal/policy's
// internal Rule representation ever does).
type ArgRule struct {
	Path  string `json:"path"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// Budget is one role's usd/minute/question caps, zero meaning "uncapped"
// (internal/store.RecordUsage's documented convention — see
// internal/api.Server.BudgetCaps's doc comment).
type Budget struct {
	USD       float64 `json:"usd"`
	Minutes   int     `json:"minutes"`
	Questions int     `json:"questions"`
}

// Subagents names a role's in-process helpers (Inline — read-only, never
// spawn a Run, per development-plan.md §5's "in-process subagents are
// permitted only for short read-only helpers") versus its spawnable
// af-subagent roles (Spawned).
type Subagents struct {
	Inline  []string `json:"inline"`
	Spawned []string `json:"spawned"`
}

// Sandbox names a role's egress allowlist entries and writable paths —
// carried through to the compiled patch for af-worktree/the egress proxy
// config; not independently enforced by this package.
type Sandbox struct {
	Network  []string `json:"network"`
	Writable []string `json:"writable"`
}

// Agent is one manifest role.
type Agent struct {
	Model     string               `json:"model"`
	Prompt    string               `json:"prompt"`
	Tools     []string             `json:"tools"`
	Deny      []string             `json:"deny"`
	ArgRules  map[string][]ArgRule `json:"arg_rules"`
	Subagents Subagents            `json:"subagents"`
	Sandbox   Sandbox              `json:"sandbox"`
	Budget    Budget               `json:"budget"`
}

// Fanout is the manifest's optional override of today's process-wide
// fanout.Caps default (cmd/control-plane/main.go: {3, 4, 12}).
type Fanout struct {
	MaxDepth          *int `json:"max_depth"`
	MaxChildrenPerRun *int `json:"max_children_per_run"`
	MaxActiveSubtree  *int `json:"max_active_subtree"`
}

// Manifest is a fully parsed, schema-valid, cross-field-valid
// .agentfleet/project.yaml.
type Manifest struct {
	Agents map[string]Agent `json:"agents"`
	Fanout Fanout           `json:"fanout"`
}

// defaultFanout is today's hardcoded cmd/control-plane/main.go value — a
// manifest that omits `fanout:` entirely gets exactly what every project
// gets today, not an unbounded default.
var defaultFanout = fanout.Caps{MaxDepth: 3, MaxChildrenPerRun: 4, MaxActiveSubtree: 12}

// Parse validates src against the embedded schema, decodes it, and runs the
// cross-field checks a JSON Schema cannot express: D15 (reviewer/implementer
// model-family separation), every `prompt:` resolving in
// internal/domain/prompts, and no role naming a hard-denied tool
// (internal/policy.hardDenyTools — mirrored here since that map is
// unexported). It returns EVERY issue found; a non-empty Issue slice means
// the returned Manifest is the zero value and must not be used.
func Parse(src []byte) (Manifest, []Issue) {
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return Manifest{}, []Issue{{Message: "invalid YAML: " + err.Error()}}
	}

	raw, err := nodeToAny(&root)
	if err != nil {
		return Manifest{}, []Issue{{Message: "invalid YAML: " + err.Error()}}
	}

	if valErr := compiledSchema.Validate(raw); valErr != nil {
		return Manifest{}, schemaIssues(valErr)
	}

	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return Manifest{}, []Issue{{Message: "internal error re-encoding parsed YAML: " + err.Error()}}
	}

	var m Manifest
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return Manifest{}, []Issue{{Message: "internal error decoding parsed YAML: " + err.Error()}}
	}

	if issues := crossFieldIssues(m); len(issues) > 0 {
		return Manifest{}, issues
	}

	return m, nil
}

// hardDenyTools mirrors internal/policy's unexported map of the same name —
// a manifest listing one of these in `tools:` is a manifest bug (D3: no
// merge tool may ever exist for any role), rejected here so it never even
// reaches internal/policy.Evaluate's own runtime hard_deny check.
var hardDenyTools = map[string]bool{
	"merge":       true,
	"gh_pr_merge": true,
}

func crossFieldIssues(m Manifest) []Issue {
	var issues []Issue

	promptNames := make(map[string]bool)
	for _, n := range prompts.Names() {
		promptNames[n] = true
	}

	for role, agent := range m.Agents {
		for _, t := range agent.Tools {
			if hardDenyTools[t] {
				issues = append(issues, Issue{
					Path:    "/agents/" + role + "/tools",
					Message: fmt.Sprintf("tool %q is never permitted for any role (docs/adr/0003) — remove it from the manifest", t),
				})
			}
		}

		if agent.Prompt != "" && !promptNames[agent.Prompt] {
			issues = append(issues, Issue{
				Path:    "/agents/" + role + "/prompt",
				Message: fmt.Sprintf("prompt %q is not in the prompt library (internal/domain/prompts)", agent.Prompt),
			})
		}
	}

	// D15 (docs/adr/0015): a reviewer and an implementer-ish role must not
	// share a model family. "Family" is the substring before the first
	// '-' — good enough for "deepseek-v4-pro" vs "claude-opus-5" without
	// this package needing a model registry.
	if reviewer, ok := m.Agents["reviewer"]; ok {
		reviewerFamily, _, _ := strings.Cut(reviewer.Model, "-")

		for role, agent := range m.Agents {
			if role == "reviewer" {
				continue
			}

			family, _, _ := strings.Cut(agent.Model, "-")
			if family == reviewerFamily {
				issues = append(issues, Issue{
					Path: "/agents/reviewer/model",
					Message: fmt.Sprintf(
						"reviewer and %s both use model family %q — D15 (docs/adr/0015) requires the reviewer to run a different model family from the implementer",
						role, family,
					),
				})
			}
		}
	}

	return issues
}

// Policy projects m into internal/policy's runtime Manifest — one
// policy.Role per manifest role, MediatedTools narrowed to the subset of
// `tools:` that internal/policy actually gates (mediatedTools above); a
// role's local tools (read/write/bash/...) pass straight through dsh's own
// tool schema and are never evaluated here.
func (m Manifest) Policy() policy.Manifest {
	roles := make(map[string]policy.Role, len(m.Agents))

	for name, agent := range m.Agents {
		var mt []string

		for _, t := range agent.Tools {
			if mediatedTools[t] {
				mt = append(mt, t)
			}
		}

		var argRules map[string][]policy.Rule
		if len(agent.ArgRules) > 0 {
			argRules = make(map[string][]policy.Rule, len(agent.ArgRules))

			for tool, rules := range agent.ArgRules {
				converted := make([]policy.Rule, len(rules))
				for i, r := range rules {
					converted[i] = policy.Rule{Path: r.Path, Op: policy.Op(r.Op), Value: r.Value}
				}

				argRules[tool] = converted
			}
		}

		roles[name] = policy.Role{MediatedTools: mt, Deny: agent.Deny, ArgRules: argRules}
	}

	return policy.Manifest{Roles: roles}
}

// Caps returns role's budget.Caps, or the zero value (uncapped, per
// internal/store.RecordUsage's convention) if the manifest declares none —
// callers wanting today's hardcoded {8, 45, 3} fallback for an unknown role
// do that themselves, the same way internal/api.Server already falls back
// to its own process-wide BudgetCaps.
func (m Manifest) Caps(role string) budget.Caps {
	agent, ok := m.Agents[role]
	if !ok {
		return budget.Caps{}
	}

	return budget.Caps{USD: agent.Budget.USD, Minutes: agent.Budget.Minutes, Questions: agent.Budget.Questions}
}

// FanoutCaps returns the manifest's fanout override, falling back field-by-
// field to defaultFanout (today's process-wide cmd/control-plane/main.go
// value) for anything the manifest didn't set.
func (m Manifest) FanoutCaps() fanout.Caps {
	caps := defaultFanout

	if m.Fanout.MaxDepth != nil {
		caps.MaxDepth = *m.Fanout.MaxDepth
	}

	if m.Fanout.MaxChildrenPerRun != nil {
		caps.MaxChildrenPerRun = *m.Fanout.MaxChildrenPerRun
	}

	if m.Fanout.MaxActiveSubtree != nil {
		caps.MaxActiveSubtree = *m.Fanout.MaxActiveSubtree
	}

	return caps
}

func nodeToAny(node *yaml.Node) (any, error) {
	if node == nil {
		return nil, nil
	}

	var v any
	if err := node.Decode(&v); err != nil {
		return nil, err
	}

	return v, nil
}

var englishPrinter = message.NewPrinter(language.English)

func schemaIssues(err error) []Issue {
	valErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Issue{{Message: err.Error()}}
	}

	var issues []Issue

	collectLeafIssues(valErr, &issues)

	if len(issues) == 0 {
		issues = append(issues, Issue{Message: valErr.Error()})
	}

	return issues
}

func collectLeafIssues(e *jsonschema.ValidationError, out *[]Issue) {
	if len(e.Causes) == 0 {
		path := "/" + strings.Join(e.InstanceLocation, "/")

		msg := "validation failed"
		if e.ErrorKind != nil {
			msg = e.ErrorKind.LocalizedString(englishPrinter)
		}

		*out = append(*out, Issue{Path: path, Message: msg})

		return
	}

	for _, c := range e.Causes {
		collectLeafIssues(c, out)
	}
}
