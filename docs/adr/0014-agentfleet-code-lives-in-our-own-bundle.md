# 0014. Everything AgentFleet-specific lives in our own dsh bundle

Status: accepted
Decision: development-plan.md D14

## Context

`dsh-base` is vendored, upstream, pre-release code. If AgentFleet-specific behavior
gets implemented by editing `dsh-base` config rows or source directly, every upstream
sync becomes a manual three-way merge against our own patches — exactly the upgrade
pain the M4.5 drill exists to catch early.

## Decision

Everything AgentFleet-specific lives in `dsh-bundle-agentfleet`
(`runner/bundle/cordis.patch.yml` + the `af-*` packages in `runner/packages/`), which
stacks over `dsh-base` + `dsh-headless` as a Cordis patch/overlay. **Patching a
`dsh-base` config row by id requires architect sign-off and a written justification**
— it is not a decision an individual contributor makes silently under deadline
pressure.

## Consequences

- A `dsh-base` upgrade is, in the common case, a submodule bump plus re-running
  `dsh --profile agentfleet-runner --dump-config` to confirm every `af-*` row still
  appears and no Cordis fiber sits `PENDING` — not a merge conflict resolution.
- Any exception (an actual `dsh-base` patch) is traceable: it needs a named architect's
  sign-off recorded in the PR, exactly like the community-plugin source-review
  requirement in §9. Two similar policies, same underlying concern: unreviewed code or
  edits inside the vendored, security-sensitive dependency.
- Together with D13, this is what keeps D12's "no Claude Agent SDK" bet from becoming a
  trap: the AgentFleet-specific surface is small, self-contained, and independently
  testable, so reverting or swapping the underlying harness is a bundle-level change,
  not an archaeological dig through a forked `dsh-base`.
