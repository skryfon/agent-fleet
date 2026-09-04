# AgentFleet

Self-hosted agentic SDLC framework. Full design: [`development-plan.md`](development-plan.md)
(v0.3, plan of record). Agent-facing conventions: [`.claude/CLAUDE.md`](.claude/CLAUDE.md).

**Status: M0–M6 done** (see `development-plan.md` §7 for the milestone
sequence and `docs/adr/` for the 17 locked decisions each milestone builds
on). The control plane, supervisor, Zulip bridge, policy/approval/budget
plumbing (including `af-context`'s D8 hash-pinned spec_ref enforcement),
orchestration (`af-subagent`, drift metric), and the manifest compiler for
multi-project support are all live. `webapp/` (approval queue, live run view
over SSE, cost/metrics dashboards) is built (M7's deliverables exist) but
**M7's own done-condition — "the team keeps the live view open during a
working day" — hasn't happened yet**, so M7 isn't claimed as done. **M8
(planning ingestion) remains** — deferred until M1–M7 have run in daily use
for a month (`development-plan.md` §7). `dsh web` remains available as a
free supplementary debug view over individual sessions.

## How it works

1. **Project setup** — a human edits `.agentfleet/project.yaml`; the control
   plane compiles it into a generated `dsh --patch` overlay and registers the
   project. Never hand-edit the generated patch.
2. **Task assignment** — an architect runs Spec Kit locally (planning only,
   D5) and lands `tasks.md` via PR. The control plane validates it and
   ingests one task row per line item, under a Zulip topic per feature.
3. **Execution loop** — once a task is queued, `supervisor` spawns a
   disposable runner container over the rootless Podman socket. The `dsh`
   agent works inside a git worktree; local tools (read/write/bash) run
   in-container, but anything crossing a boundary — asking a human, spawning
   a subagent, opening a PR — is mediated through the control-plane API and
   logged as an event. No merge tool exists anywhere in the system.
4. **Human interaction** — workers can only `ask_orchestrator`; only the
   orchestrator gets `ask_human` (D7), which posts to the feature's Zulip
   topic and blocks the run. Timeouts never auto-answer: nudge at 4h,
   escalate at 24h, park the task at 72h.
5. **Hosting** — one Podman Compose machine, four network zones (edge, core,
   runners, egress). Runner containers are `internal: true` — no path to
   Postgres, Zulip, or the Podman socket; the egress proxy is their only
   route to GitHub and model providers, and it rejects PR-merge calls.

A fuller diagram of this flow — swimlanes, the human-interaction API surface,
and the deployment topology — is at
[`docs/diagrams/agentfleet-workflow.html`](docs/diagrams/agentfleet-workflow.html)
(open in a browser).

## Prerequisites

- Go 1.26+
- [Podman](https://podman.io/) + `podman compose` (D11 — not Docker)
- Node 22.19+ / 24+, pnpm 11+
- `DEEPSEEK_API_KEY` (used by the runner from M1 onward)

## Local development

```sh
git clone --recurse-submodules <this repo>   # deepseek-harness/ is a pinned submodule
cp .env.example .env   # fill in ZULIP_SITE/ZULIP_BOT_EMAIL/ZULIP_BOT_API_KEY

make up          # postgres + migrate (one-shot) + control-plane + caddy
curl localhost:8080/healthz   # direct
curl localhost:8081/healthz   # through caddy

make down
```

Zulip is Zulip Cloud (hosted), not a local service — no compose target for
it. See `deploy/zulip/README.md` for org/bot/identity setup.

Migrations apply automatically via the one-shot `migrate` service in
`deploy/compose.yaml`; `make migrate-up`/`migrate-down` remain for running them
directly against `DATABASE_URL` if you have `golang-migrate` installed.

```sh
go build ./...
go test ./...
go run ./cmd/control-plane
```

The `runner/` pnpm workspace builds the `agentfleet-runner` dsh profile
(`dsh-base` + `dsh-headless` + `dsh-bundle-agentfleet`) from the `af-*`
Cordis plugins — see [`runner/README.md`](runner/README.md) for plugin
status and `pnpm typecheck`/`pnpm build`/`npx vitest run` from `runner/`.
Before editing an `af-*` package, work through
[`docs/cordis-ramp.md`](docs/cordis-ramp.md) and the `dsh-plugin-dev` skill.

## What's next (M8)

M8 (moving Spec Kit into the harness for headless planning) is explicitly
deferred until M1–M7 have run in daily use for a month
(`development-plan.md` §7).

## Repo layout

See `development-plan.md` §2. Short version: `cmd/` + `internal/` (Go
control plane, supervisor, bridge — policy, budget, outbox, fanout, podman,
redact, questions, store all implemented), `runner/` (dsh profile + `af-*`
plugins), `webapp/` (approval queue, live run view, cost/metrics dashboards
— implemented, M7), `deploy/` (compose, migrations, Dockerfiles, Zulip Cloud
setup docs),
`deepseek-harness/` (vendored `dsh`, pinned submodule), `docs/adr/` (18
files, one per locked decision plus the README), `docs/diagrams/` (workflow
diagram), `docs/upgrade-drills/` (dsh version-bump records).
