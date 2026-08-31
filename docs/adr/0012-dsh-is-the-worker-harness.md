# 0012. DeepSeek Harness (dsh) is the worker harness — no Claude Agent SDK

Status: accepted
Decision: development-plan.md D12

## Context

The original plan called for building a runner on `@anthropic-ai/claude-agent-sdk`.
Two independently authored plans both reached the same alternative call
(development-plan.md §0): `dsh` already ships, as swappable Cordis extension points,
most of what a hand-built runner would need — session log with crash recovery, a tool
pipeline with a policy waterfall, a mock-LLM test harness (`packages/test-support/
llm-mock-server`), subagent delegation, approvals, sandboxing, and a one-shot headless
runner bundle (`dsh-headless`). Building all of that from scratch on the Agent SDK
would duplicate work dsh has already done.

## Decision

`dsh` (DeepSeek Harness), vendored as a version-pinned git submodule at
`deepseek-harness/`, is the worker harness. There is no Claude Agent SDK anywhere in
this repo. AgentFleet's runner is a `dsh` *profile* (`agentfleet-runner` = `dsh-base` +
`dsh-headless` + `dsh-bundle-agentfleet`), composed from Cordis extension points
(`tools/pre-execute`, `agent/pre-step`, `agent/turn-stopping`, subagent provider), not
a custom application.

## Consequences

- **This is the central technical risk of the whole plan.** `dsh` is pre-release
  (`SESSION_FORMAT_VERSION = 0`, packages `private: true`, no compatibility promise).
  M1 carries an explicit kill criterion: if `af-policy` denial requires patching
  `dsh-base` itself, or the M1 spike isn't working after two weeks, revert to the
  Claude Agent SDK plan — do not push through.
- The vendored checkout is never auto-upgraded (`.claude/CLAUDE.md`); M4.5 exists
  specifically to measure the cost of a version bump before five projects depend on
  it, and that drill must not be skipped.
- This decision only holds together in combination with D13 (control plane never
  imports a dsh type) and D14 (AgentFleet code lives in its own bundle, never patches
  `dsh-base` by id) — see those ADRs. D12 without D13 and D14 is how a
  developer-preview dependency becomes unupgradeable (§1).
- Community `dsh` plugins are unvetted and run arbitrary code with GitHub credentials
  inside a runner; nothing community-sourced runs there without a named person's
  source review recorded in the adding PR (§9).
