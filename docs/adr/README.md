# ADRs

One file per locked decision in `development-plan.md` §1, named
`NNNN-short-slug.md`. Written in M0 per development-plan.md §7.

| ADR | Decision |
|---|---|
| [0001](0001-postgres-owns-lifecycle-dsh-session-owns-context.md) | D1 — Postgres owns cross-run lifecycle state; dsh session log owns model-visible context |
| [0002](0002-one-container-one-worktree-per-run.md) | D2 — One container per Run, one git worktree per Run |
| [0003](0003-agents-cannot-merge-prs.md) | D3 — Agents cannot merge PRs, enforced at four layers |
| [0004](0004-zulip-as-primary-human-channel.md) | D4 — Zulip, self-hosted, primary human channel from M0/M3 |
| [0005](0005-spec-kit-for-planning-only.md) | D5 — Spec Kit is used for planning only |
| [0006](0006-planning-runs-on-the-architects-laptop.md) | D6 — Planning runs on the architect's laptop until M8 |
| [0007](0007-only-the-orchestrator-asks-humans.md) | D7 — Workers get `ask_orchestrator`; only the orchestrator gets `ask_human` |
| [0008](0008-context-by-hash-pinned-reference.md) | D8 — Context passes by hash-pinned reference, never by transcript |
| [0009](0009-single-machine-compose-no-kubernetes.md) | D9 — Single self-hosted machine, Podman Compose, no Kubernetes |
| [0010](0010-no-temporal-state-machine-and-outbox.md) | D10 — No Temporal; state machine + transactional outbox |
| [0011](0011-podman-not-docker.md) | D11 — Podman, not Docker, as the container runtime |
| [0012](0012-dsh-is-the-worker-harness.md) | D12 — DeepSeek Harness (dsh) is the worker harness; no Claude Agent SDK |
| [0013](0013-control-plane-never-imports-a-dsh-type.md) | D13 — The control plane never imports a dsh type |
| [0014](0014-agentfleet-code-lives-in-our-own-bundle.md) | D14 — Everything AgentFleet-specific lives in our own dsh bundle |
| [0015](0015-reviewers-use-a-different-model-family.md) | D15 — Reviewer agents use a different model family from implementer agents |
| [0016](0016-egress-proxy-terminates-tls.md) | M4 — The egress proxy terminates TLS to filter the merge endpoint by path |
| [0017](0017-manifest-compiles-to-a-dsh-patch.md) | M6 — `.agentfleet/project.yaml` compiles to a generated, per-run dsh `--patch` overlay |

## Template

```md
# NNNN. Title

Status: accepted
Decision: development-plan.md D<n>

## Context

## Decision

## Consequences
```
