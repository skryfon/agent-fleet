---
description: Create a git commit with a conventional message
argument-hint: [message]
allowed-tools: Bash(git add:*), Bash(git status:*), Bash(git commit:*), Bash(git diff:*)
model: haiku
---

## Staged changes

!`git diff --cached`

## Task

Create a commit following Conventional Commits. If $ARGUMENTS is provided, use it
as the summary; otherwise infer a concise summary from the staged diff above.
