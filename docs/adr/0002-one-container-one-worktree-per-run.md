# 0002. One container per Run, one git worktree per Run

Status: accepted
Decision: development-plan.md D2

## Context

Multiple runs — a worker task, a reviewer pass on the same task, parallel subagent
work under an orchestrator (M5) — can be in flight against the same repository at
once. They must not corrupt each other's working tree, and a crashed or killed run
must not leave the repository in an inconsistent state for another run to inherit.

## Decision

Every Run gets its own container (§2: "disposable, one container per Run") and its own
git worktree, never shared. `supervisor` creates and tears down both per Run.

## Consequences

- Worktree lifecycle (creation, cleanup on success/failure/timeout, orphan reaping) is
  `af-worktree`'s job and needs to be correct under crash — a killed container must
  not leak worktrees indefinitely.
- Disk cost scales with concurrent runs, not with active features. Acceptable at the
  stated ceiling of 3–4 concurrent runs (§8) — human review capacity, not compute, is
  the limiting factor.
- Rules out any optimization that shares a checkout between runs (e.g. a warm worktree
  pool) unless it also guarantees isolation before handing a worktree to a new Run —
  not attempted in v1.
- Makes "one container per Run" the natural unit for cost/token accounting
  (`run.tokens_in`, `run.cost_usd` in the schema) and for the four-layer merge
  prevention story: a Run's container is where a rogue merge attempt would originate,
  and it's the unit that egress-proxy filtering and Podman capabilities are applied to.
