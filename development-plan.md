# AgentFleet — Implementation Plan v0.3

Unifies two independently authored v0.2 plans (this document's prior revision, and
`new-devplan.md`) into a single plan of record. Self-contained.

A self-hosted agentic SDLC framework for a 5–6 person team. Humans initiate tasks in
Zulip. Sandboxed DeepSeek Harness (`dsh`) workers pull code, branch, implement, test
and open PRs. An orchestrator coordinates multi-agent work and is the only agent that
talks to humans. Agents never merge.

---

## 0. What changed, and why (merge notes)

Both prior plans independently reached the same core call: replace the planned
`@anthropic-ai/claude-agent-sdk` runner with `dsh` (deepseek-harness), because `dsh`
already ships — as swappable Cordis seams — most of what the runner would have had to
build: session log with crash recovery, a tool pipeline with a policy waterfall, a
mock-LLM test harness, subagent delegation, approvals, sandboxing, and a one-shot
headless runner bundle. Where they diverged, this version picks explicitly:

| Point of divergence | Resolution | Why |
|---|---|---|
| Container runtime | **Podman**, no socket-proxy | Rootless/daemonless by default — no host-wide privileged daemon to front with a proxy sidecar. Retained from the original locked D11 rather than reverting to Docker + `docker-socket-proxy`. |
| Human channel | **Zulip from M0/M3**, not deferred | Zulip is the team's actual working surface (D4), not a placeholder to be upgraded later. `dsh web` is still used for free as an internal debug view, but is not a milestone deliverable. |
| Runner internals | `dsh-headless` bundle + eight granular `af-*` plugins (`af-control`, `af-policy`, `af-context`, `af-ask-human`, `af-budget`, `af-worktree`, `af-github`, `af-subagent`) | Reuses dsh's shipped one-shot runner instead of hand-building boot/exit logic; splits responsibilities along dsh's own extension points (`tools/pre-execute`, `agent/pre-step`, `agent/turn-stopping`, subagent provider) rather than one monolithic boot plugin. |
| Manifest mechanism | `.agentfleet/project.yaml` **compiles to a generated `dsh --patch` overlay**, snapshot-tested | Concrete, testable mechanism for D9-style "role determines tool access" instead of leaving it implicit in preset composition. |
| Postgres/session-log relationship | Postgres mirrors durable `dsh` session events **idempotently and one-way**; the dsh session log stays the byte-authoritative source within a run | Gives cross-run audit/search in Postgres without inventing a second source of truth. The mirror is derived data, never the thing replayed from. |
| Extra governance | Added: model-family diversity for reviewers (D14), a mid-plan upgrade drill (M4.5), a community-plugin policy (§9) | None of these existed in the first draft; both are cheap and address real risks (shared model blind spots, alpha-dependency upgrade cost, unvetted third-party plugin code running with credentials). |
| GitHub webhook ingestion (`af-webhook`) | Kept as an **optional, deferred (M6+) plugin**, not part of the core roster | dsh ships this exact worked example (`dsh-webhook-github`) for auto-starting review sessions on `ready_for_review`; useful, but not required for the core task-execution loop. |

---

## 1. Decisions of record

| # | Decision |
|---|---|
| D1 | Postgres owns cross-run lifecycle state. The dsh session log owns model-visible context within a run and is the byte-authoritative source for that run. A one-way, idempotent mirror copies durable dsh session events into Postgres for cross-run audit and search — it is derived data, never a second source of truth. |
| D2 | One container per Run, one git worktree per Run. |
| D3 | Agents cannot merge PRs. Enforced at four layers; branch protection is the one that actually works. |
| D4 | Zulip Cloud (hosted), free tier. One topic per **feature**. Primary human channel from M0/M3. Amended from self-hosted 2026-08-31 — see `docs/adr/0004-zulip-as-primary-human-channel.md`. |
| D5 | Spec Kit is used for **planning only** — never for implementation. |
| D6 | Until M8, planning runs on the architect's laptop and lands as a PR of spec artifacts. |
| D7 | Workers get `ask_orchestrator`. Only the orchestrator gets `ask_human`. |
| D8 | Context passes by hash-pinned reference, never by transcript. |
| D9 | Single self-hosted machine, **Podman** Compose. No Kubernetes. |
| D10 | No Temporal in v1. State machine + transactional outbox + reconciler. |
| D11 | **Podman, not Docker, as the container runtime.** Rootless and daemonless by default: no host-wide privileged daemon sitting on the machine, so no `docker-socket-proxy`-style sidecar is needed — least privilege comes from the rootless socket being per-user and mounted only into `supervisor`. |
| D12 | **DeepSeek Harness (`dsh`) is the worker harness. No Claude Agent SDK.** |
| D13 | **The control plane never imports a dsh type.** All contact is the HTTP API in §4. |
| D14 | **Everything AgentFleet-specific in dsh lives in our own bundle.** Patching a `dsh-base` config row by id requires architect sign-off and a written justification. |
| D15 | **Reviewer agents use a different model family from implementer agents.** Shared blind spots between reviewer and implementer defeat the point of review. |

D12–D14 exist together. D12 without D13 and D14 is how a developer-preview dependency
becomes unupgradeable.

**Handoff contract.** `tasks.md`, produced by Spec Kit and committed via PR, is the
entire interface between planning and execution. Schema pinned by an org preset,
validated before ingestion.

---

## 2. Architecture

```
                        ┌──────────────┐
   Zulip  ◄────────────►│ zulip-bridge │
                        └──────┬───────┘
   Web app ◄──────────► ┌──────▼────────┐      ┌────────────┐
   CLI     ◄──────────► │ control-plane │◄────►│  Postgres  │
                        └──────┬────────┘      └────────────┘
                               │ launch
                        ┌──────▼────────┐
                        │  supervisor   │──► rootless podman socket (user-scoped)
                        └──────┬────────┘
                               │ spawn
                  ┌────────────▼─────────────┐    ┌──────────────┐
                  │ dsh runner (N)            │───►│ egress-proxy │──► providers, github
                  │ profile: agentfleet-run   │    └──────────────┘
                  │ dsh-base + dsh-headless   │
                  │ + dsh-bundle-agentfleet   │
                  └───────────────────────────┘
```

`dsh web` remains available as a free, zero-build internal debug view over any running
or completed session (it ships with the harness); it is not a milestone deliverable and
is not the team's supervision surface — Zulip is.

| Service | Language | Responsibility |
|---|---|---|
| `control-plane` | Go | State machine, policy, API, event log, budgets. Only writer to Postgres. |
| `supervisor` | Go | Runner container lifecycle. **Only** service with Podman access. |
| `runner` | dsh + TS bundle | Agent execution. Disposable, one container per Run. |
| `zulip-bridge` | Go | Zulip ↔ control-plane translation. Stateless. |
| `webapp` | React | Approval queue, live view, timelines, dashboards. |

`supervisor` is separate because the control plane is network-facing and must never
hold Podman access, and the runner must never see a socket at all.

### Repositories

```
agentfleet/                      # monorepo
├── cmd/{control-plane,supervisor,bridge}/
├── internal/{domain,policy,budget,store,api}/
├── runner/                      # dsh bundle + profile, pnpm workspace
│   ├── packages/af-control/
│   ├── packages/af-policy/
│   ├── packages/af-context/
│   ├── packages/af-ask-human/
│   ├── packages/af-budget/
│   ├── packages/af-worktree/
│   ├── packages/af-github/
│   ├── packages/af-subagent/
│   ├── packages/af-webhook/      # optional, M6+
│   └── bundle/                  # dsh-bundle-agentfleet
├── webapp/
├── deploy/{compose.yaml,caddy,egress-proxy,migrations}/
├── deepseek-harness/             # vendored, version-pinned, never auto-upgraded
└── docs/adr/                    # one file per decision above

agentfleet-presets/               # Spec Kit org preset (tasks.md contract)
agentfleet-prompts/                # versioned role prompts
```

---

## 3. Data model

Postgres. `golang-migrate` + `sqlc`.

```sql
project     id, slug, manifest_ref, manifest_hash, repos[], status
feature     id, project_id, slug, spec_ref, zulip_topic, state
task        id, feature_id, external_ref, lane, title, intent,
            acceptance_criteria jsonb, touches[], depends_on[],
            spec_refs jsonb,        -- {path, anchor, sha256}
            state, assignee
run         id, task_id, parent_run_id, role, model, container_id,
            dsh_session_id, state, checkpoint, tokens_in, tokens_out, cost_usd
event       id, run_id, task_id, seq, kind, actor, payload jsonb, at
            -- append-only; idempotent mirror of dsh session/event plus
            -- control-plane-native decisions. Derived, never authoritative
            -- for in-run model context — the dsh session log owns that.
artifact    id, task_id, kind, uri, sha256
question    id, run_id, task_id, kind, body, options jsonb, addressee,
            state, answer, answered_by, asked_at, answered_at
approval    id, subject_kind, subject_ref, subject_sha256, decision, actor, decided_at
budget      scope_kind, scope_id, usd_cap, minute_cap, question_cap, *_spent
identity    id, kind, display_name, zulip_user_id, github_login, role
outbox      id, topic, payload jsonb, published_at
```

Three invariants:

- `event` is append-only, never sampled, never expired. It is the audit trail.
- `approval.subject_sha256` is mandatory. A revised artifact voids its approval.
- State changes and their outbound effects commit in one transaction via `outbox`.

### Task state machine

```
CREATED ─► QUEUED ─► RUNNING ─► REVIEW ─► DONE
             ▲          │
             └──────────┼─► BLOCKED_ON_HUMAN ─┘
                        └─► FAILED / CANCELLED / PARKED
```

Table-driven in `internal/domain`. Every transition writes an event. Illegal
transitions error, never silently no-op.

---

## 4. Control plane API

```
POST /v1/projects                        register project + manifest ref
POST /v1/features                        open feature, create Zulip topic
POST /v1/features/{id}/tasks:ingest      validate + load tasks.md
POST /v1/tasks/{id}/start | /cancel

# runner-facing, internal network only
POST /v1/runs/{id}/events                batched session-event mirror (af-control)
POST /v1/runs/{id}/tools/{name}          mediated tool → policy → execute
POST /v1/runs/{id}/checkpoint
GET  /v1/runs/{id}/inbox                 long-poll: answers, cancellations

# human-facing
POST /v1/questions/{id}/answer
POST /v1/approvals                       {subject_ref, sha256, decision}
GET  /v1/tasks?state=... | /v1/events?since=...   (SSE)
POST /v1/admin/pause                     global + per-project kill switch
```

Local tools (read, write, bash inside the worktree) execute in the runner for latency.
Mediated tools (spawn, ask, PR creation, anything crossing a boundary) go through the
API so the decision is recorded as an event. This is D13 in practice: the control plane
never imports a dsh type, and every dsh-side effect that matters crosses this API.

---

## 5. The dsh runner

Profile `agentfleet-runner` = `dsh-base` + `dsh-headless` + `dsh-bundle-agentfleet`.
`dsh-headless` is the shipped one-shot runner bundle — it owns process boot and exit,
so AgentFleet plugins never reimplement that lifecycle.

| Plugin | Seam | Responsibility |
|---|---|---|
| `af-control` | `session/event`, HTTP | Mirrors events; polls run inbox |
| `af-policy` | `tools/pre-execute` | Denies merge, protected push, out-of-scope write; emits `policy_violation` |
| `af-context` | `agent/pre-step` | Resolves hash-pinned `spec_refs` (D8); rejects the step on hash mismatch |
| `af-ask-human` | `ctx.tools` + `agent.inject()` | `ask_human` / `ask_orchestrator`; injects answers on resume |
| `af-budget` | telemetry + `agent/turn-stopping` | Token/cost/minute caps; stops the turn on breach |
| `af-worktree` | `ctx.fs`, `ctx.subprocess` | Confines all IO to `/workspace/<run-id>` |
| `af-github` | `ctx.tools` | Branch, commit, push, PR via `gh`. **No merge tool exists.** |
| `af-subagent` | subagent provider | `spawn_worker` routes through the control plane |
| `af-webhook` *(optional, M6+)* | `ctx.webhookRuntime` | Auto-starts a read-only review session on GitHub `ready_for_review`, modeled on dsh's own `github-review` example |

Two rules that are easy to get wrong:

**Allow-list first.** A role's manifest declares its tools; unregistered tools are
absent from the tool schema, so the model never sees them. `af-policy` is the second
line, for tools that exist but are contextually forbidden.

**Implementation subagents must be spawned, not in-process.** A dsh in-process child
agent is invisible to the control plane — no Run row, no budget, no depth limit, no
separate PR. In-process subagents are permitted only for short read-only helpers,
declared per role.

### Manifest compiles to a patch

`.agentfleet/project.yaml` stays the human contract. At run start the control plane
compiles it to a dsh `--patch` overlay. Generated, never hand-edited.

```yaml
agents:
  implementer:
    model: deepseek-v4-pro
    tools: [read, write, bash, git, gh_pr_create, ask_orchestrator, report]
    subagents: { inline: [search, summarize], spawned: [reviewer] }
    sandbox: { network: [github.com, proxy.golang.org], writable: [workspace] }
    budget: { usd: 8, minutes: 45, questions: 3 }
```

Snapshot-test the output of `dsh --profile agentfleet-runner --dump-config` in CI. A
silent config-layering change on upgrade is precisely the breakage the alpha-dependency
warning describes.

### Model allocation

| Role | Model class | Rationale |
|---|---|---|
| Orchestrator | Strongest reasoning available | Planning errors compound |
| Implementer | DeepSeek V4 Pro or equivalent | Bulk token consumer |
| Reviewer | **Different family from implementer (D15)** | Same model shares blind spots |
| Classifier / summarizer | Cheapest adequate | High volume, low stakes |

Before routing production code anywhere, confirm your data-handling obligations permit
the provider and decide as a company whether source leaving your network to a given
jurisdiction is acceptable. Architect call, not an engineering one. The `ctx.llm` seam
makes a self-hosted open-weight fallback, or a second provider for D15, cheap either
way.

---

## 6. Human interaction

Two tools, never merged into one:

```
ask_human(question, kind, options?, context_ref?)    # unblocks a run
request_approval(subject, artifact_ref, sha256)      # gates a transition
```

**An answer is data. It can never expand tool scope or change policy.** An agent asking
"should I merge?" and hearing "yes" still cannot merge.

### Zulip mechanics

- **One open question per topic at a time.** Queue the rest. Eliminates answer
  ambiguity and rate-limits interruption.
- Emoji reaction for `choice`/`confirm`; topic reply for `free_text`; `/answer <id>
  <text>` as escape hatch.
- Address by role — requirements to the architect, implementation to the assigned
  developer.
- Verify the sender maps to a known `identity`. Unmapped senders are ignored and
  logged.

### Durability

The runner polls the inbox for five minutes, then checkpoints and **exits the
container**. The `question` row is the durable state. On answer, the control plane
resurrects a runner, resumes the dsh session, and `af-ask-human` injects the answer via
`agent.inject()`.

Timeouts never auto-answer: nudge at 4h, escalate at 24h, park the task at 72h.

### Budgets as signal

Cap questions per run (3) and per feature (10). Breach parks the feature and tells the
architect the spec was underspecified. Do not respond by raising the cap.

---

## 7. Milestones

~1.5 FTE. This is a platform build alongside product work.

### M0 — Foundations and Cordis ramp (3 weeks)
Compose stack (Podman): Postgres, Caddy. (Langfuse deferred to M7.) Monorepo, CI,
migrations. ADRs for D1–D15. Zulip Cloud org set up, bots created, identities mapped
(D4 amended from self-hosted — `docs/adr/0004-zulip-as-primary-human-channel.md`).

**Two engineers work the Cordis primer, tutorial and extension cookbook.** Two, not
one — this knowledge must not be single-homed.

**Done when:** `podman compose up` yields a control plane answering `/healthz`, a bot
account on Zulip Cloud can send/receive via the API, and both engineers can explain
the dsh boot tree from `--dump-config`.

### M1 — dsh runner spike and first PR (2 weeks) ⚠ highest risk
Write `af-github` (`create_branch` first), `af-worktree`, and `af-policy`. Run under
`dsh-headless` in a rootless Podman container against the Go/Gin repo.

**Prove `af-policy` denial works from a plugin, without patching `dsh-base`, before
building anything on top of it.** That capability is the whole reason to prefer dsh.

**Done when:** a real task produces a mergeable PR a developer merges by hand, and a
deliberately requested forbidden tool is denied and logged. Record wall-clock and cost
as your baseline.

**Kill criterion:** if denial requires a `dsh-base` patch, or the spike isn't working
after two weeks, revert to the Claude Agent SDK plan. Do not push through.

### M2 — Control plane (3 weeks)
Domain model, state machine, event log, outbox, `tasks.md` ingestion with schema
validation, mediated tool dispatch, supervisor service, `af-control` session-event
mirror. CLI becomes a thin API client.

**Build the fake-LLM runner here.** dsh already ships one (`packages/test-support/
llm-mock-server`, `pnpm mock:llm`) — point it at a scripted tool sequence and it lets
you integration-test the control plane, policy, questions and orchestration
deterministically at zero token cost. Highest-leverage item in the plan, and it's
already built.

**Done when:** tasks flow QUEUED → RUNNING → REVIEW with a complete event trail, and
killing the control plane mid-run loses nothing.

### M3 — Zulip bridge and ask_human (2 weeks)
`af-ask-human`, question lifecycle, checkpoint-and-exit, resurrect-and-resume with
`agent.inject()`. One open question per topic. Timeout ladder.

**Done when:** an agent asks, its container exits, and the run resumes correctly six
hours later after a Zulip reply.

### M4 — Policy, approvals, safety (2 weeks)
Hash-bound approvals with Zulip actions. Four-layer merge prevention: branch protection
+ CODEOWNERS, dedicated GitHub App per project, `af-policy` denial, egress-proxy
filtering of `PUT /repos/*/pulls/*/merge`. `af-budget` with hard kill. Secret redaction
on all emitted events, tested with a canary string in CI. Kill switch.

**Done when:** a misbehaving prompt attempting a merge is blocked at three layers, the
fourth verified by manual test, and the violation reaches Zulip within seconds.

### M4.5 — Upgrade drill (0.5 week)
Bump dsh one minor version. Fix the bundle. Measure the cost.

Do not skip this. It is the cheapest possible test of your central risk (dsh is a
pre-release, `SESSION_FORMAT_VERSION = 0`, no compatibility promise), and you want the
number before five projects depend on the thing.

### M5 — Orchestration (4 weeks)
Orchestrator role, `af-subagent` `spawn_worker` with depth and fan-out limits,
`ask_orchestrator` routing, `report_to_orchestrator`, per-feature aggregate budgets,
subtree cancellation. Deviation reporting and drift counter.

**Done when:** a feature fans out to parallel tasks, the orchestrator is the only agent
reaching a human, and cancelling the parent kills the subtree.

### M6 — Manifest, multi-project, and optional webhook ingestion (3 weeks)
Manifest schema, loader, patch compilation, `--dump-config` snapshot tests, versioned
prompt library, per-project credential isolation and network namespaces. Optionally
land `af-webhook` for auto-started review sessions on GitHub `ready_for_review`.

**Done when:** a second project onboards via a manifest and a GitHub App installation,
zero framework code changes.

### M7 — Web app and observability (2 weeks)
Approval queue with full diff context, live run view over SSE, cost dashboards by
feature/role/model, drift and question-rate metrics. `dsh web` remains available as a
free supplementary debug view over individual sessions; it does not replace this work.

**Done when:** the team keeps the live view open during a working day.

### M8 — Planning ingestion (deferred, 3 weeks)
Move Spec Kit into the harness: headless invocation, `/speckit.clarify` questions
through the M3 machinery, approval gates on `spec.md` and `plan.md`,
architect-triggered `converge` re-planning.

**Do not start until M1–M7 have been in daily use for a month.**

**Total ≈ 24.5 weeks.** The Cordis ramp and upgrade drill roughly offset what dsh saves
on the runner and UI. You are buying architecture, not schedule.

**Stop-after-M4.5 is a legitimate outcome.** A supervised single-agent system with real
safety rails delivers most of the value; M5–M7 are what you add once the team trusts
it.

---

## 8. Deployment

### Machine
8 vCPU / 32 GB / 500 GB SSD. Long-lived services use 6–8 GB. **Cap concurrency at 3–4
runs.** The ceiling is human review capacity, not CPU.

### Compose topology

```yaml
networks:
  edge:      # caddy ↔ webapp
  core:      # control-plane, postgres, bridge, langfuse, supervisor
  runners:   # internal: true — no direct egress
  egress:

services:
  caddy:          [edge]
  webapp:         [edge, core]
  control-plane:  [core]
  bridge:         [core]            # reaches Zulip Cloud outbound only, no inbound route
  postgres:       [core]
  langfuse:       [core]
  supervisor:     [core]            # rootless podman.sock, mounted only here
  egress-proxy:   [runners, egress]
  # runners created dynamically on [runners] with HTTP_PROXY set
```

No socket-proxy service: Podman's rootless, daemonless model means there is no
host-wide privileged daemon to front with one. `supervisor` runs as its own
unprivileged user with a rootless Podman socket (`podman system service`)
bind-mounted into it and nowhere else.

`runners` is `internal: true`. A runner's only routes out are the control-plane API
and the egress proxy. No path to the Podman socket, Postgres, or Zulip Cloud.

### Egress allowlist
```
api.deepseek.com   api.anthropic.com   github.com   api.github.com
proxy.golang.org   sum.golang.org      registry.npmjs.org   pypi.org
```
`api.anthropic.com` is listed to support D15 (a different model family for reviewers)
if that family is Anthropic's; drop it if the second family is self-hosted instead.
Filter `api.github.com` by method and path; reject merge endpoints. Denials log as
`policy_violation`.

### Runner container
```
rootless (Podman) · read-only rootfs · tmpfs /tmp · writable volume at /workspace
no capabilities · seccomp default · no host mounts
limits: 2 CPU, 4 GB, pids-limit
HTTP_PROXY set, no direct internet DNS
GitHub App installation token injected at start, 1h expiry
node + pnpm + pinned dsh version (vendored deepseek-harness/ checkout) + af-* bundle
```

### Secrets
`.env` at mode 600 owned by a dedicated user; SOPS with an age key for anything
committed. Vault is operational cost without proportional benefit at this scale.

Never: secrets in the manifest, in agent context, or in events.

### Backups
Hourly `pg_dump` (7d) and daily (30d), off-machine. **Test the restore quarterly.** An
untested backup is a hypothesis. Zulip is Zulip Cloud (D4 amendment) — its backups are
Zulip's operational responsibility, not ours.

### Bootstrap order
1. Machine, Podman, non-root user, firewall (443 + SSH keys only).
2. Caddy with TLS.
3. Zulip Cloud org (already hosted); create bots, map identities — see
   `deploy/zulip/README.md`. No self-hosted install step.
4. Postgres, migrations.
5. Langfuse.
6. GitHub App per project; private key to secrets; install on target repos.
7. **Branch protection on every target repo**: require PR, CODEOWNERS review, status
   checks, restrict pushes, no bypass for the App.
8. Egress proxy; verify a runner cannot reach an unlisted host.
9. control-plane, supervisor, bridge, webapp.
10. Build and pin the `dsh` runner image from the vendored checkout; verify
    `--dump-config` inside it.

Step 7 is not deferrable. It is the layer that actually prevents merges.

---

## 9. Community plugins

The `dsh-plugin` ecosystem is weeks old, unvetted, and executes arbitrary code inside
your runner.

- **Nothing community-sourced runs in a runner holding GitHub credentials** until a
  named person has read the source; record the review in the adding PR.
- Pin exact versions. Lockfile committed. No ranges.
- Vendor anything you depend on materially.
- Prefer writing 200 lines yourself over adopting a plugin that touches filesystem,
  network or credentials.
- Community plugins are fine for unprivileged capabilities — formatters, renderers,
  telemetry exporters — in profiles that never see production credentials.

Assume a supply-chain incident within a year and design so it is survivable: scoped
short-lived token, no socket, allowlisted egress, cannot merge.

---

## 10. Team

| Role | Ownership |
|---|---|
| Architect A | Domain model, state machine, policy engine, ADRs |
| Architect B | Spec Kit preset, `tasks.md` contract, role prompts, model allocation |
| Developer 1 | control-plane, store, supervisor |
| Developer 2 | **dsh bundle** (all eight core `af-*` plugins plus `af-webhook`), Zulip bridge |
| DevOps | Compose (Podman), egress proxy, secrets, backups, branch protection, CI |
| Designer | Approval queue, live run view, run timeline |

Developer 2 plus one architect take the Cordis ramp in M0. Rotate a weekly "framework
on-call" who triages agent failures and feeds them into the eval dataset; without a
named owner this decays.

---

## 11. Metrics

Instrument from M2, review weekly.

- **Time-to-review** per PR — if it rises, reduce concurrency.
- **Drift rate** — deviations reported per task. Rising means specs are too thin.
- **Question rate** per run and feature — rising during implementation means planning
  underperformed.
- **Cost per merged PR** — the only cost number that means anything.
- **Policy violations** — should trend to zero.
- **Lane distribution** — if 90% bypasses the spec lane, SDD adoption is cosmetic.
- **dsh upgrade cost** — engineer-hours per version bump, from M4.5 onward.
  First data point (dsh-v0.1.2-alpha.2 → alpha.3, `docs/upgrade-drills/`):
  under 1 hour for the bump itself (zero typed drift, zero composition drift).
  A separate ~2-hour cost surfaced during the same drill — a pre-existing,
  environment-local container-build issue, confirmed unrelated to the bump by
  reproducing it against the old pin too — not counted against dsh's
  compatibility record.

---

## 12. Risks

| Risk | Mitigation | Kill criterion |
|---|---|---|
| dsh breaking changes (alpha, `SESSION_FORMAT_VERSION = 0`, no compatibility promise) | D13/D14 seams, own bundle, pinned version, M4.5 drill | Two consecutive upgrades over a week each → freeze and vendor |
| Cordis learning curve | M0 ramp, two engineers | M1 spike failing after two weeks → revert to Agent SDK |
| Policy not expressible as a plugin | M1 proves it first | Same as above — this is the reason to use dsh |
| Community plugin supply chain | Source review, pinning, vendoring, unprivileged profiles | Any incident → community code loses all privileged profiles |
| Review becomes the bottleneck | Concurrency cap, time-to-review metric | Time-to-review exceeds agent runtime → stop adding agents |
| Cost overruns | Hard caps, kill on breach | Cost per merged PR exceeds a developer-hour → pause and re-scope |
| Provider policy conflict on source code | Decide before M1; `ctx.llm` seam makes swapping cheap | — |
| Framework becomes a second job | Explicit 1.5 FTE, stop-after-M4.5 option | Product slips two sprints → freeze at current milestone |
| Prompt injection via issue/spec content | Allow-listed tools, egress filtering, branch protection, all ingested text untrusted | — (structural defence, not detective) |
| Reviewer/implementer share a model family, defeating review | D15: enforce different families at manifest level | — |

The most likely failure is not technical: it is building M5–M7 before M1–M4 have been
used in anger for a month. Ship each milestone into daily use before starting the next.

---

## 13. Week one

1. Write ADRs for D1–D15. Half a day; prevents months of re-litigation.
2. Provision the machine, Podman, Compose skeleton, Postgres, Caddy.
3. Set up Zulip Cloud org, create bots, map identities (D4 amendment — hosted, not
   self-installed).
4. **Turn on branch protection for the Go/Gin repo now**, before any agent has
   credentials.
5. `npx @deepseek-ai/dsh web` locally; run a task on a scratch repo (this is a debug
   aid, not the target supervision surface — see §2).
6. `dsh --profile headless --dump-config`; read the tree until it makes sense.
7. Cordis primer + the "adding a tool" cookbook entry.
8. Write `af-github` with a single `create_branch` tool on `ctx.tools`.
9. Write `af-policy`: a `tools/pre-execute` listener that denies one named tool and
   emits an event.

If item 9 works from a plugin without patching `dsh-base`, the plan holds. If it
doesn't, stop and tell the team — the worker layer gets reconsidered before anything is
built on it.
