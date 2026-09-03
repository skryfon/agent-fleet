package manifest

import "gopkg.in/yaml.v3"

// patchRow is one cordis config-tree row, the same shape
// runner/bundle/cordis.patch.yml itself uses. Marshaled with yaml.v3 from a
// struct rather than string concatenation, so quoting the manifest's own
// strings is the library's problem, not ours.
type patchRow struct {
	ID     string         `yaml:"id"`
	Config map[string]any `yaml:"config"`
}

// policyBaselineDeny mirrors runner/packages/af-policy's own
// DEFAULT_CONFIG.deny (src/index.ts) — af-policy's apply() does
// `{...DEFAULT_CONFIG, ...config}`, a SHALLOW spread, so a patch that sets
// `deny` replaces the default array outright rather than adding to it.
// Patch always includes this baseline in the union it emits so a manifest
// can only ADD deny entries here, never accidentally drop af-policy's own
// floor. Keep this in sync with af-policy's DEFAULT_CONFIG by hand — same
// documented coupling as internal/policy.hardDenyTools mirroring
// af-policy's deny list in the other direction.
var policyBaselineDeny = []string{"merge", "gh_pr_merge"}

// Patch compiles role's manifest entry into a dsh --patch overlay: the
// per-run layer that sits ON TOP of runner/bundle/cordis.patch.yml (which
// keeps owning plugin composition — D14, docs/adr/0014 — this never patches
// a dsh-base row). deploy/runner-entrypoint.sh writes the result to a
// tmpfs file and passes `--patch <file>` alongside `--profile
// agentfleet-runner`.
//
// What it does NOT do: drive af-budget's usd/minute/question caps — those
// are enforced by the control plane itself (internal/api.resolveManifest,
// internal/store.RecordUsage), not by runner-side config; af-budget's own
// Config knobs (usdPerMillionTokens, reportIntervalMs) are pricing/cadence,
// a different concern the manifest schema doesn't currently expose.
//
// Returns an error if role isn't declared in the manifest — callers
// (internal/supervisor.Handlers.RunLaunch) must resolve the role against
// m.Agents before launching, same as internal/policy.Evaluate already
// fails closed on an unknown role.
func (m Manifest) Patch(role string) ([]byte, error) {
	agent, ok := m.Agents[role]
	if !ok {
		return nil, unknownRoleError(role)
	}

	rows := []patchRow{
		{
			ID:     "af-policy",
			Config: map[string]any{"deny": dedupe(append(append([]string{}, policyBaselineDeny...), agent.Deny...))},
		},
	}

	// af-subagent's spawnedRoles (runner/packages/af-subagent/src/index.ts):
	// which roles THIS role's spawn_worker call may target, straight from
	// the manifest's subagents.spawned list. Omitted (not an empty array)
	// when the manifest declares none, so af-subagent's own "unset means
	// unrestricted" default holds for a role that never mentions subagents.
	if len(agent.Subagents.Spawned) > 0 {
		rows = append(rows, patchRow{
			ID:     "af-subagent",
			Config: map[string]any{"spawnedRoles": agent.Subagents.Spawned},
		})
	}

	return yaml.Marshal(rows)
}

func dedupe(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))

	for _, it := range items {
		if !seen[it] {
			seen[it] = true

			out = append(out, it)
		}
	}

	return out
}

type unknownRoleErr struct{ role string }

func (e unknownRoleErr) Error() string { return "manifest: role " + e.role + " is not declared" }

func unknownRoleError(role string) error { return unknownRoleErr{role: role} }
