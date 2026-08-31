# 0004. Zulip as the primary human channel from M0/M3

Status: amended 2026-08-31 (see Amendment below) — hosting model changed from
self-hosted to Zulip Cloud; the rest of the original decision stands.
Decision: development-plan.md D4

## Context

Agents need to ask humans questions, request approvals, and report progress somewhere
the team already works — not a bespoke dashboard nobody checks. Zulip's topic model
(threaded conversations within a stream) maps naturally onto "one open question per
feature" without inventing a UI. The team is ≤10 people, well inside Zulip's free
self-hosted tier.

## Decision

Zulip, self-hosted, is the primary human channel, live from **M0/M3** — not deferred
behind the web app. One topic per **feature** (not per task, not per run). `dsh web`
remains available as a free, zero-build internal debug view over any session, but is
explicitly not the supervision surface and not a milestone deliverable.

## Consequences

- M0 must stand up the actual Zulip instance (`--push-notifications`, org, bots,
  identity mapping) — it is infrastructure to operate from day one, including backups
  (`manage.py backup`, tested restore quarterly per §8), not a stub to build later.
- `af-ask-human` (M3) and the human-facing question/approval API (§4) are designed
  against Zulip's topic and message model from the start, rather than against a
  hypothetical webapp UI that doesn't exist until M7.
- The webapp (M7) is deliberately scoped to what Zulip and `dsh web` *don't* cover
  (approval queue, cost dashboards) rather than re-implementing either.
- Only the orchestrator reaches Zulip (see D7 / `0007-only-the-orchestrator-asks-humans.md`)
  — workers never post directly, keeping one open question per topic instead of a
  flood of per-worker threads.

## Amendment (2026-08-31): Zulip Cloud instead of self-hosted

**Context for the amendment.** Self-hosting was attempted first, per the original
decision above. Standing up the real upstream `docker-zulip` compose topology (five
services: app, postgres, redis, rabbitmq, memcached) surfaced two environment-specific
failures during M0 local verification on Apple Silicon under Podman's `applehv` machine
backend: the app container's native arm64 build hit `Illegal instruction (core
dumped)` from a CPU-feature-detection gap in a Rust-backed Python wheel, and the amd64
build (tried under Rosetta translation to route around that) hit a subsequent
`chmod: Permission denied` on its own container filesystem — plausibly a rootless
Podman + Rosetta + virtiofs storage interaction. Both are laptop-virtualization
artifacts, not application bugs, but chasing them further wasn't a good use of time
against a team that already holds a free Zulip Cloud account.

**Decision.** Use Zulip Cloud (`https://<org>.zulipchat.com`) instead of self-hosting.
Everything else in this ADR (one topic per feature, D7's orchestrator-only human
contact, `dsh web` as a separate free debug view) is unchanged.

**Consequences.**
- No local Zulip compose stack, no self-hosted backup/restore obligation for Zulip
  specifically (§8's backup cadence still applies to Postgres) — Zulip Cloud operates
  its own infrastructure and its own backups.
- No self-hosted push-notification bouncer registration (`--push-notifications`,
  `manage.py register_server`); Zulip Cloud runs its own push notification service.
- **New tradeoff this introduces**: conversation content — including whatever agents
  post as questions, spec excerpts, or progress reports — now lives on a third-party
  SaaS rather than on our own machine, unlike every other piece of this system's data
  (D1 Postgres, D2 worktrees, D9 single machine). This is the same category of judgment
  call as D15's model-provider data-handling question: an explicit, accepted tradeoff
  for this team's threat model at this scale, not an oversight. Revisit if the
  project's data-handling requirements tighten.
- Free-plan limits to design around (checked 2026-08, zulip.com/plans): 10,000
  messages of searchable history, 5 GB file storage, ~120 API requests/minute per bot.
  None are binding at a 5–6 person team's scale.
- The bridge (`cmd/bridge`, M3) integrates via Zulip's plain REST + real-time events
  API (`POST /api/v1/register` once, then long-poll `GET /api/v1/events`) rather than
  an outgoing-webhook bot — this needs no public HTTPS endpoint, consistent with the
  bridge living on the `core` network making only outbound connections. No maintained
  Go Zulip client exists, so this is a thin hand-rolled `net/http` wrapper, not an
  added dependency. See `deploy/zulip/README.md` §6 for the full mechanism.
