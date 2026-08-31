# AgentFleet — Agent Guide

## What this is

Self-hosted agentic SDLC framework: sandboxed **DeepSeek Harness (`dsh`)** workers pull
code, branch, implement, test, and open PRs; humans supervise via Zulip; agents never
merge. Full design in `development-plan.md` (v0.3, unified plan of record) — read it
before architectural changes.

`dsh` is an all-plugin Cordis agent harness, vendored at `deepseek-harness/` and
version-pinned — never auto-upgraded. AgentFleet's runner is a `dsh` profile
(`agentfleet-runner` = `dsh-base` + `dsh-headless` + `dsh-bundle-agentfleet`), not a
custom application. There is **no Claude Agent SDK** anywhere in this repo.

## Repo layout (monorepo, per development-plan.md §2)

- `cmd/control-plane/`, `cmd/supervisor/`, `cmd/bridge/`, `internal/` — Go
- `runner/` — dsh bundle + profile, pnpm workspace: `packages/af-control`,
  `af-policy`, `af-context`, `af-ask-human`, `af-budget`, `af-worktree`, `af-github`,
  `af-subagent`, `af-webhook` (optional, M6+), plus `bundle/` (`dsh-bundle-agentfleet`)
- `webapp/` — React (approval queue, live view, timelines, dashboards)
- `deploy/` — compose.yaml, caddy, egress-proxy, migrations
- `deepseek-harness/` — vendored `dsh` checkout, version-pinned
- `docs/adr/` — one file per locked decision (D1–D15)

## Commands

- Go: `go build ./...`, `go test ./...`, `golangci-lint run` (lint not wired yet — M2)
- runner (`runner/`): `pnpm install`, `pnpm run typecheck`, `pnpm run build`,
  `npx vitest run`
- Composition smoke test: from a target repo,
  `node <path-to>/deepseek-harness/apps/cli/lib/bin.js --profile agentfleet-runner
  --dump-config` — every `af-*` row must appear, no Cordis fiber may sit `PENDING`.
  **Do not run `dsh` via `pnpm dsh` from outside `deepseek-harness/`** — pnpm scripts
  always execute with cwd pinned to the package root, which silently breaks any tool
  (e.g. `create_branch`) that shells out relative to the invoking directory. Use the
  built `apps/cli/lib/bin.js` directly instead; see `runner/README.md`.
- LLM: model requests route through OmniRoute (`http://localhost:20128`) via
  `@deepseek-ai/dsh-llm-pi-ai`'s `omni-route` route, already declared in
  `$DSH_HOME/settings.yaml` — configuration, not a plugin. No `af-llm-*` package
  exists or is needed; see the M1 plan for the full writeup.
- webapp: `npm test`, `npm run build` (in `webapp/`)

## Locked decisions (do not relitigate without flagging it)

- **DeepSeek Harness (`dsh`) is the worker harness. No Claude Agent SDK. (D12)**
- The control plane never imports a dsh type — all contact is the HTTP API in
  `development-plan.md` §4. (D13)
- Everything AgentFleet-specific in `dsh` lives in our own bundle
  (`dsh-bundle-agentfleet`); patching a `dsh-base` config row by id requires architect
  sign-off. (D14)
- Reviewer agents use a different model family from implementer agents — shared
  blind spots defeat the point of review. (D15)
- Postgres owns cross-run lifecycle state; the dsh session log owns model-visible
  context within a run and is byte-authoritative for it. Postgres gets a one-way,
  idempotent mirror of durable dsh session events for cross-run audit/search — derived
  data, never a second source of truth. (D1)
- One container per Run, one git worktree per Run.
- Agents cannot merge PRs — enforced at four layers (branch protection is the real
  one).
- Context passed by reference (hash-pinned), never by transcript.
- No Temporal, no Kubernetes, no ORM — single Podman Compose machine, `sqlc`, plain
  SQL.
- Podman, not Docker — rootless, daemonless; no privileged host socket to guard, no
  socket-proxy sidecar.
- `control-plane` never calls an LLM — stays deterministic and cheap to test.
- Zulip is the primary human channel from M0/M3, not a later addition. `dsh web` is
  available as a free internal debug view but is not the supervision surface.
- Full list: `development-plan.md` §1 (D1–D15).

## Conventions

- `event` table is append-only — never update or delete rows in it.
- `approval.subject_sha256` is mandatory; a revised artifact voids its approval.
- State transitions are table-driven in `internal/domain`, not scattered `if` chains;
  every transition writes an `event`.
- Policy engine (`internal/policy`) is a pure function `(role, tool, args, manifest) →
  allow | deny(reason)` — no side effects, golden-test it.
- Every `af-*` package is an ordinary Cordis plugin: `export const name`,
  `export const inject`, `export function apply(ctx, config)`; registrations are
  effects (`ctx.effect()`, or a registry's returned disposer). Follow
  `deepseek-harness/docs/cookbook/adding-a-package.md` and `adding-a-tool.md`.
- `.agentfleet/project.yaml` is the human-edited manifest; it compiles to a generated
  `dsh --patch` overlay. Never hand-edit the generated patch.
- Allow-list first, `af-policy` second: a role's manifest declares its tools so
  unregistered ones never reach the model; `af-policy`'s `tools/pre-execute` denial is
  the line for tools that exist but are contextually forbidden.
- In-process dsh subagents are for short read-only helpers only. Anything that needs a
  Run row, a budget, a depth limit, or its own PR must be spawned via `af-subagent`,
  not run in-process — an in-process child is invisible to the control plane.

## Gotchas

- `runners` network is `internal: true` — no path to Postgres, Zulip, or the Podman
  socket from a runner container. Only `supervisor` touches the (rootless) Podman
  socket.
- Egress proxy allowlist additionally filters `api.github.com` to reject
  `PUT /repos/*/pulls/*/merge` — don't loosen this without updating the four-layer
  merge-prevention story in development-plan.md §M4.
- Secrets never enter agent context or `event` payloads — redaction filter applies to
  every emitted event; test it with a canary string.
- `dsh` is pre-release (`SESSION_FORMAT_VERSION = 0`, no compatibility promise,
  packages `private: true`). Never bump the vendored checkout without running the M4.5
  upgrade drill and re-checking `--dump-config`.
- Community `dsh` plugins are unvetted and execute arbitrary code inside the runner.
  Nothing community-sourced runs in a runner holding GitHub credentials without a
  named person's source review recorded in the adding PR. See development-plan.md §9.

## Subagents and skills

Reach for these before doing the work solo — they carry pre-loaded context and
narrower tool access that keeps diffs and reviews on-target.

| Agent (`.claude/agents/`) | Use for |
|---|---|
| `engineering-multi-agent-systems-architect` | Designing or reviewing `af-*` plugin topology/coordination (agent-to-agent trust, failure/fallback paths, HITL gates) — invokes the `dsh-plugin-dev` skill itself before touching Cordis extension points |
| `engineering-minimal-change-engineer` | Implementing a scoped `af-*`/Go fix without scope creep — bug fixes, small features |
| `engineering-git-workflow-master` | Branching, rebasing, stacked PRs, commit hygiene |
| `engineering-devops-automator` | compose.yaml, CI pipelines, Podman/egress-proxy automation |
| `engineering-database-reliability-engineer` | Postgres migrations, failover, backup/PITR for the control-plane DB |
| `code-reviewer` | Any non-trivial diff, before committing |
| `security-appsec-engineer` | Threat modeling / secure-code review of a new surface (e.g. a new tool exposed to agents) |
| `security-cloud-security-architect` | Podman/network/deploy topology review (zero-trust, egress allowlist) |
| `security-secrets-credential-engineer` | Credential handling, `.env`/vault flow, redaction-filter changes |
| `security-ai-generated-code-auditor` | Scan-fix-rescan pass after a large AI-generated diff |

Skills (`.claude/skills/`, invoke via the Skill tool):

- **`dsh-plugin-dev`** — before writing or editing any `af-*` runner package, tool, or
  hook plugin: Cordis plugin shape, event dispatch, extension-point selection, and
  the AgentFleet-specific constraints (D14, `runners` network isolation, in-process
  subagent limits) in one place instead of re-deriving them from
  `deepseek-harness/docs/` each time.
- **`golang-lint`**, **`golang-security`**, **`golang-database`**,
  **`golang-concurrency`**, **`golang-continuous-integration`**,
  **`golang-observability`** — vendored from
  [samber/cc-skills-golang](https://github.com/samber/cc-skills-golang) (MIT) into
  `.claude/skills/` for the `cmd/`, `internal/` Go modules. Mapped to the relevant
  agent per the table above via each agent file's own `## Skills` section
  (`code-reviewer`, `engineering-minimal-change-engineer`,
  `engineering-database-reliability-engineer`, `engineering-devops-automator`,
  `security-appsec-engineer`, `security-ai-generated-code-auditor`). Third-party
  content — review before bumping past the vendored snapshot.

## Pointers

- Full plan: `development-plan.md` (v0.3)
- ADRs (once written): `docs/adr/`
- dsh docs: `deepseek-harness/docs/` — start with `architecture.md` and
  `cordis-primer.md` before touching `runner/`
