# AgentFleet

Self-hosted agentic SDLC framework. Full design: [`development-plan.md`](development-plan.md)
(v0.3, plan of record). Agent-facing conventions: [`.claude/CLAUDE.md`](.claude/CLAUDE.md).

This is the M0 scaffold — see `development-plan.md` §7 for the milestone
sequence. Most of what's here is a skeleton with `TODO` markers pointing at
the milestone that fills it in.

## Prerequisites

- Go 1.26+
- [Podman](https://podman.io/) + `podman compose` (D11 — not Docker)
- Node 22.19+ / 24+, pnpm 11+
- `DEEPSEEK_API_KEY` (from M1 onward; not needed yet)

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

Go services build with the standard toolchain, no vendored/network
dependencies yet:

```sh
go build ./...
go run ./cmd/control-plane
```

The `runner/` pnpm workspace (`af-*` Cordis plugins) isn't wired to dsh yet —
see [`runner/README.md`](runner/README.md) for what M1 adds. Before writing
runner code, work through [`docs/cordis-ramp.md`](docs/cordis-ramp.md) — M0's
Cordis ramp checklist and `--dump-config` exercise.

## What M0 leaves for M1

The compose stack, migrations, ADRs (`docs/adr/`), CI, and Cordis ramp are M0.
Nothing in `runner/packages/af-*` depends on `@deepseek-ai/cordis` yet, there's
no `agentfleet-runner` profile, and `internal/{api,domain,policy,store,budget}`
are package skeletons with no logic — all of that is M1+ (`development-plan.md`
§7).

## Repo layout

See `development-plan.md` §2. Short version: `cmd/` + `internal/` (Go
control plane, supervisor, bridge), `runner/` (dsh profile + plugins),
`webapp/` (approval queue + dashboards, M7), `deploy/` (compose, migrations,
Dockerfiles, Zulip Cloud setup docs), `deepseek-harness/` (vendored `dsh`,
pinned submodule), `docs/adr/` (one file per locked decision),
`docs/cordis-ramp.md` (M0 Cordis ramp checklist).
