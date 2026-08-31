# 0010. No Temporal in v1 — state machine plus transactional outbox

Status: accepted
Decision: development-plan.md D10

## Context

Temporal (or a similar workflow engine) would give durable execution and retries for
free, but it is another service to run, another failure domain to reason about, and
another team-wide skill to ramp — on top of the Cordis ramp M0 already requires. The
task/run lifecycle here (§3: `CREATED → QUEUED → RUNNING → REVIEW → DONE`, with
`BLOCKED_ON_HUMAN`/`FAILED`/`CANCELLED`/`PARKED` branches) is a small, enumerable state
machine, not an arbitrary long-running workflow DAG.

## Decision

No Temporal. Durability comes from a table-driven state machine in `internal/domain`
(pure `(state, event) -> (state, effects)`, every transition writes an `event` row,
illegal transitions error rather than silently no-op) plus a transactional `outbox`
table so a state change and its outbound side effects (a Zulip message, a runner
launch) commit atomically in one Postgres transaction.

## Consequences

- "Killing the control plane mid-run loses nothing" (M2's done-when) has to be earned
  by the outbox pattern and idempotent effect delivery, not handed to us by a workflow
  engine's own durability guarantees. This is the specific thing to test hardest in M2.
- The state machine stays table-driven, not scattered `if` chains, specifically so it
  remains auditable and golden-testable without needing a workflow-engine UI to
  visualize it (development-plan.md §7 conventions, `.claude/CLAUDE.md`).
- If task orchestration ever needs true long-running-workflow features (dynamic
  sub-workflow trees with cross-cutting compensation, say) beyond what M5's
  `af-subagent` depth/fan-out limits and subtree cancellation provide, that is a
  reason to revisit — not before.
