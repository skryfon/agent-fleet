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
cp .env.example .env

make up          # postgres + control-plane via podman compose
curl localhost:8080/healthz

make migrate-up  # apply deploy/migrations/ (requires golang-migrate)
make down
```

Go services build with the standard toolchain, no vendored/network
dependencies yet:

```sh
go build ./...
go run ./cmd/control-plane
```

The `runner/` pnpm workspace (`af-*` Cordis plugins) isn't wired to dsh yet —
see [`runner/README.md`](runner/README.md) for what M1 adds.

## Repo layout

See `development-plan.md` §2. Short version: `cmd/` + `internal/` (Go
control plane, supervisor, bridge), `runner/` (dsh profile + plugins),
`webapp/` (approval queue + dashboards, M7), `deploy/` (compose, migrations,
Dockerfiles), `deepseek-harness/` (vendored `dsh`, pinned submodule),
`docs/adr/` (one file per locked decision).
