---
description: Review the current changes for quality, security, and tests
allowed-tools: Read, Grep, Glob, Bash(git diff:*)
---

## Changed files

!`git diff --name-only origin/HEAD...`

## Detailed diff

!`git diff origin/HEAD...`

## Instructions

Review the changes above. Report issues by priority (blocking → nits) covering:
correctness, security, error handling, test coverage, and readability. Cite file:line.

Pay special attention to this project's non-negotiables (see CLAUDE.md and
development-plan.md §1): agents must never be able to merge PRs, the `event` table must
stay append-only, secrets must never enter agent context or emitted events, and the
control-plane must never call an LLM directly.
