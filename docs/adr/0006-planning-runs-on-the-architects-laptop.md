# 0006. Planning runs on the architect's laptop until M8

Status: accepted
Decision: development-plan.md D6

## Context

Ingesting planning artifacts automatically (e.g. from a webhook or a planning service)
is real integration work, and until M1–M7 have proven out in daily use there's nothing
concrete yet to validate that integration against. Building it early would be
speculative.

## Decision

Until M8, Spec Kit planning runs on the architect's laptop, and its output
(`tasks.md`, specs) lands in the target repo as an ordinary PR. M8 (automated planning
ingestion) is explicitly deferred and gated: "do not start until M1–M7 have been in
daily use for a month" (development-plan.md §7).

## Consequences

- No planning-ingestion service, webhook, or API exists before M8 — `tasks.md` arrives
  by the same PR-and-review path as any other change, using the schema validation
  called for in the handoff contract (D5).
- "Stop after M4.5" is a legitimate outcome per §7 — this decision means that outcome
  doesn't leave planning automation half-built and orphaned; it simply never started.
- When M8 does start, it is ingesting a contract (`tasks.md`'s schema) that has already
  been exercised manually for months, not a hypothesis.
