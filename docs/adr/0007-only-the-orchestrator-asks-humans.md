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
- Reinforces D4's one-topic-per-feature Zulip model — without this, a fanned-out
  feature would need one topic per worker instead of one per feature.

## Implementation (M5)

Enforced the same way as every other tool grant, allow-list first:
`cmd/control-plane/main.go`'s process-wide manifest gives `orchestrator`
`ask_human`/`spawn_worker`/`answer_worker` and gives `implementer`
`ask_orchestrator`/`report_to_orchestrator` — **not** `ask_human`. `internal/policy`
is the second line, gating on whichever tool name is actually dispatched.

That second line only works because the tool name reaching it is real: `af-ask-human`
(`runner/packages/af-ask-human`) originally dispatched the literal string `'ask_human'`
for both tools it registers, so a worker's `ask_orchestrator` call was evaluated by
`internal/policy` as `ask_human` and denied outright — a real bug, caught while writing
this ADR's own enforcement test, fixed by threading the actual tool name through.

`internal/store.ApplyAsk` picks `TrAskedOrchestrator` over `TrAsked` when the request
carries a `ToRunID` (the orchestrator run): that trigger schedules no `zulip.question`
effect, and instead enqueues a `run_inbox` row (kind `worker_question`) the orchestrator
reads via its own `check_workers` tool. `question_one_open_per_feature_uk` is scoped
`WHERE to_run_id IS NULL` so worker→orchestrator questions never contend with the
feature's human-facing slot, or with each other beyond `question_one_open_per_run_uk`
(one open ask_orchestrator per worker at a time).

Verified end-to-end by `internal/api/m5_integration_test.go`'s
`TestAskOrchestratorRoutesToParentRunNotZulip`: a worker's `ask_human` still 403s, its
`ask_orchestrator` succeeds and provably enqueues no `zulip.question` outbox row, and
the orchestrator's own inbox delivers the question.
