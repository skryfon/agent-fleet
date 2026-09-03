# 0017. `.agentfleet/project.yaml` compiles to a generated, per-run dsh `--patch` overlay

Status: accepted
Decision: development-plan.md §5, §7 M6

## Context

Through M5, every mediated-tool policy, budget cap, and fan-out cap was hardcoded
process-wide in `cmd/control-plane/main.go`, each carrying a `TODO(M6)` pointing here.
Onboarding a second project meant editing Go and redeploying — the opposite of M6's
own done-criterion ("a second project onboards via a manifest and a GitHub App
installation, zero framework code changes").

The manifest also needs to reach the runner container itself, not just the control
plane: a role's `deny` list and which roles it may spawn are runner-side plugin
config (`af-policy`, `af-subagent`), not something the control plane can enforce by
itself from outside the process.

## Decision

`.agentfleet/project.yaml` is parsed, schema-and-cross-field-validated, and stored as
compiled JSON on `project.manifest` at registration time
(`internal/domain/manifest.Parse`, `POST /v1/projects` / `PUT
/v1/projects/{slug}/manifest`) — never re-parsed from YAML on the hot path. Two
independent projections come off the same compiled `Manifest`:

1. **Control-plane side**: `internal/api.resolveManifest` reads `project.manifest` per
   mediated-tool-dispatch/budget/fan-out call and projects it into
   `policy.Manifest`/`budget.Caps`/`fanout.Caps` via `Manifest.Policy`/`Caps`/
   `FanoutCaps` — replacing the process-wide fallback fields
   (`api.Server.Manifest`/`BudgetCaps`/`FanoutCaps`, kept as the no-manifest default,
   never deleted).
2. **Runner side**: `Manifest.Patch(role)` compiles the role's entry into a small
   cordis overlay (only `af-policy`'s `deny` and `af-subagent`'s `spawnedRoles` today —
   the two runner-side knobs a manifest actually needs to drive; budget caps are
   enforced by the control plane, not runner config). `internal/supervisor.RunLaunch`
   resolves it once per launch and hands it to the daemon as `LaunchRequest.Patch`;
   `deploy/runner-entrypoint.sh` writes it to a tmpfs file and passes
   `--patch <file>` alongside `--profile agentfleet-runner`. This layers ON TOP of
   `runner/bundle/cordis.patch.yml`, which keeps owning plugin composition — D14
   (docs/adr/0014) is untouched: no `dsh-base` row is ever patched by a manifest.

`runner/scripts/dump-config.sh --patch <file> --check` snapshot-tests the composed
result against a fixture patch (`runner/testdata/dump-config-manifest.golden`), the
same discipline D14/the M4.5 drill already apply to the unpatched profile — a dsh
upgrade that silently changes how `--patch` itself layers is exactly the kind of
alpha-dependency breakage the M4.5 drill exists to catch early, and the unpatched
golden alone can't see it.

## Consequences

- A project with no manifest (`project.manifest` still at `0006_m6.up.sql`'s `'{}'`
  default — every project registered before M6) behaves exactly as it did before M6:
  the fallback fields, no `--patch` argument, `DefaultRole`/`DefaultModel`.
- `manifest_hash` is computed server-side as `sha256(raw manifest text)` at
  registration, never trusted from the client — the same "the hash is the integrity
  claim, not a caller-supplied label" discipline `approval.subject_sha256` already
  uses.
- `internal/domain/manifest` has zero dependency on Postgres or HTTP — it is a pure
  parse/validate/project/compile package, golden-tested the same way
  `internal/domain/tasksmd` already is, so its output shape is reviewable in a diff
  rather than only observable at runtime.
- D15 (docs/adr/0015, reviewer/implementer model-family separation) is now enforced at
  manifest-registration time, not just documented policy — `manifest.Parse` rejects a
  manifest where `reviewer` shares a model family with any other role.
