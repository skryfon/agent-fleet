# 0015. Reviewer agents use a different model family from implementer agents

Status: accepted
Decision: development-plan.md D15

## Context

If the same model family both writes and reviews code, systematic blind spots (a
pattern it consistently gets wrong, a class of bug it doesn't recognize as a bug)
survive review, because the reviewer is prone to exactly the same mistake the
implementer is. Review only earns its cost if the reviewer can actually catch what
the implementer missed.

## Decision

Reviewer agents run on a different model family than implementer agents (§5's model
allocation table: implementer on "DeepSeek V4 Pro or equivalent", reviewer explicitly
called out as "different family from implementer (D15) — same model shares blind
spots"). Added as extra governance in the v0.3 merge, not present in either prior
draft (§0).

## Consequences

- The egress allowlist (§8) includes `api.anthropic.com` specifically to support this
  if the second model family is Anthropic's — drop it if the second family is
  self-hosted instead; the allowlist should reflect the actual choice made, not be left
  wider than necessary.
- The `ctx.llm` seam (§5) is what makes swapping in a second provider (or a
  self-hosted open-weight fallback) cheap; this decision is one of the reasons that
  seam exists rather than a single hardcoded model client.
- Before routing production code to any external model provider, data-handling
  obligations and jurisdiction questions are an architect call, not an engineering one
  (§5) — this decision doesn't pre-answer which second family to use, only that it
  must differ from the implementer's.
- Cost: running two model families means two sets of API relationships, rate limits,
  and potentially two billing relationships to operate — accepted because shared
  blind spots defeat the entire point of having review at all.
