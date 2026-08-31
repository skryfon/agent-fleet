# 0007. Workers get ask_orchestrator; only the orchestrator gets ask_human

Status: accepted
Decision: development-plan.md D7

## Context

If every worker could message a human directly, "one open question per topic"
(D4/§6) collapses immediately under parallel work (M5's fan-out), and humans lose a
single point of triage for what's actually blocking a feature versus what's routine
noise a coordinating agent should resolve or batch itself.

## Decision

Workers get `ask_orchestrator` — routed to whatever orchestrator is coordinating their
task, never straight to a human. Only the orchestrator gets `ask_human` /
`request_approval`. This is enforced the same way as all tool access: the role's
manifest simply never grants `ask_human` to a worker (D9-style allow-list-first from
§5), backed by `af-policy` as the second line.

## Consequences

- M5's done-when depends on this directly: "the orchestrator is the only agent reaching
  a human" is a stated acceptance criterion, not an aspiration.
- A worker with a question either gets answered by the orchestrator directly (routine)
  or has its question surfaced to Zulip *by* the orchestrator (genuinely blocking) —
  the orchestrator is a real filter, not a pass-through wire.
- Before M5 (no orchestrator role yet), this decision is dormant: single-worker runs in
  M1–M4 have no `ask_orchestrator` hop to make, and `ask_human` usage in that window is
  scoped narrowly per M3's `af-ask-human` rollout.
- Reinforces D4's one-topic-per-feature Zulip model — without this, a fanned-out
  feature would need one topic per worker instead of one per feature.
