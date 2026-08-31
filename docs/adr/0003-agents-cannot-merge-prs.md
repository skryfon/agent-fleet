# 0003. Agents cannot merge PRs

Status: accepted
Decision: development-plan.md D3

## Context

The entire premise of unattended agentic work landing on real repositories is that a
human is the only one who merges. A single enforcement point (e.g. only in the system
prompt, or only in `af-policy`) is one bug or one prompt injection away from being
bypassed. This needs defense in depth.

## Decision

Agents cannot merge PRs, enforced at four independent layers:

1. **Branch protection + CODEOWNERS** on every target repo — require PR, required
   review, status checks, restrict direct pushes, **no bypass for the GitHub App**.
2. A **dedicated GitHub App per project**, installed with permissions that do not
   include merge rights the branch protection doesn't already block.
3. **`af-policy` denial** — the tool-dispatch policy engine (development-plan.md §7)
   denies `merge` as a matter of role/manifest, before a call ever reaches GitHub.
4. **Egress-proxy filtering** of `PUT /repos/*/pulls/*/merge` at the network layer, so
   even a compromised or misconfigured runner can't reach the endpoint.

## Consequences

- **Branch protection is the layer that actually works** — it is enforced by GitHub
  itself, independent of anything running on our infrastructure. The other three are
  defense in depth, not the primary guarantee, and none of them is a substitute for
  step 7 of the §8 bootstrap order ("Step 7 is not deferrable").
- M4's done-when is explicit about this: a misbehaving prompt attempting a merge must
  be blocked at *three* layers, with the fourth verified by manual test — not just
  demonstrated once.
- Loosening the egress-proxy allowlist for `api.github.com` (development-plan.md §8)
  must not reopen the merge-endpoint filter; any change there needs to be checked
  against this ADR, per the gotcha already recorded in `.claude/CLAUDE.md`.
- This decision is orthogonal to `af-policy`'s general allow/deny role for other tools
  — merge denial is called out specifically because it is the one deny that must never
  regress silently.
