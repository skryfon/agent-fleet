# 0001. Postgres owns cross-run lifecycle state; dsh session log owns model-visible context

Status: accepted
Decision: development-plan.md D1

## Context

A Run has two kinds of state: what a human or the control plane needs to track across
runs (queue position, retries, cost, approvals, event history for audit) and what the
model needs to see to keep acting inside one run (its own transcript, tool results,
injected answers). dsh already owns the second kind — the session log is how it
recovers from a crash mid-run and how `agent.inject()` resumes a checkpointed session.
Building a second, competing transcript store in Postgres would mean two systems that
can disagree about what the model actually saw.

## Decision

Postgres owns cross-run lifecycle state: `task`, `run`, `event` (mirror), `approval`,
`question`, `budget`. The dsh session log is the byte-authoritative source for
model-visible context *within* a run. A one-way, idempotent mirror copies durable dsh
session events into the Postgres `event` table for cross-run audit and search — it is
derived data, never replayed from, never a second source of truth.

## Consequences

- `af-control` (development-plan.md §5) must make the mirror idempotent — replays after
  a crash or reconnect must not double-write `event` rows. This is a real invariant to
  test, not a formality.
- Anything that needs to resume a run's *behavior* (not just its history) resumes via
  `agent.inject()` against the dsh session, never by replaying Postgres events at the
  model.
- Postgres search/audit tooling can never be more current than the last mirrored batch
  — there is an inherent lag between what the model has seen and what Postgres has
  recorded. Acceptable for audit; not a substitute for the session log during a live
  resume.
- If dsh's session log format changes across an upgrade (see
  `0012-dsh-is-the-worker-harness.md`), the mirror's shape may need to change too —
  this is part of the cost tracked by the M4.5 upgrade drill.
