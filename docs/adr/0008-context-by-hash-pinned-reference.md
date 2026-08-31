# 0008. Context passes by hash-pinned reference, never by transcript

Status: accepted
Decision: development-plan.md D8

## Context

Passing full transcripts between runs (e.g. orchestrator → worker, or a resumed run)
bloats context, silently drifts if the underlying spec changes mid-flight, and makes
it impossible to tell whether a run acted on the spec a human actually approved.

## Decision

Context is passed by hash-pinned reference: `task.spec_refs` is
`[{path, anchor, sha256}]` (§3 schema), never an inlined copy of spec text. `af-context`
resolves these references at `agent/pre-step` and **rejects the step outright on hash
mismatch** (§5) rather than silently re-fetching newer content.

## Consequences

- A spec edit after a task is queued does not retroactively change what a running
  task sees — the run either keeps acting on the pinned version or fails loudly, never
  drifts unnoticed. This mirrors `approval.subject_sha256`'s "a revised artifact voids
  its approval" invariant (§3) — the same discipline applied to context instead of
  approvals.
- Every task carries an auditable, exact pointer to the spec version it was built
  against, useful for both debugging and post-hoc review.
- Cost: re-queuing after a spec edit isn't automatic — someone (a human, or eventually
  the orchestrator) has to notice the mismatch and requeue with fresh refs. Accepted
  because silent drift is worse than an explicit stop.
- `af-context`'s rejection path needs to be a clean, recognizable failure mode (not a
  generic tool error) so operators can tell "spec moved under me" apart from other
  run failures.
