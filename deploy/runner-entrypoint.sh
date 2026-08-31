#!/usr/bin/env bash
# agentfleet-runner container entrypoint. Clones REPO_URL once into a
# persistent base checkout, carves this Run its own `git worktree` (D2), then
# execs dsh from inside it — so dsh-fs-local's `cwd` default (evaluated once,
# at module import, before af-worktree's apply() ever runs) is already
# correct. See runner/packages/af-worktree/src/index.ts for why worktree
# CREATION lives here instead of in that plugin.
set -euo pipefail

: "${REPO_URL:?REPO_URL is required, e.g. https://github.com/<owner>/<repo>.git}"
: "${RUN_ID:?RUN_ID is required (a run-unique id, e.g. a uuid)}"
: "${TASK:?TASK is required (the prompt handed to the headless agent)}"
: "${GH_TOKEN:?GH_TOKEN is required by af-github push/gh_pr_create (never baked into the image)}"

# `gh` reads GH_TOKEN itself for API calls; this additionally points git's own
# credential helper at it so plain `git clone`/`git push` authenticate too.
gh auth setup-git
git config --global user.name "${AGENTFLEET_GIT_NAME:-agentfleet-runner}"
git config --global user.email "${AGENTFLEET_GIT_EMAIL:-agentfleet-runner@users.noreply.github.com}"

BASE_REPO_DIR="$AGENTFLEET_WORKSPACE_ROOT/base"
WORKTREE_DIR="$AGENTFLEET_WORKSPACE_ROOT/$RUN_ID"
BRANCH="agentfleet/$RUN_ID"

if [ -d "$BASE_REPO_DIR/.git" ]; then
  git -C "$BASE_REPO_DIR" fetch origin
else
  git clone "$REPO_URL" "$BASE_REPO_DIR"
fi

git -C "$BASE_REPO_DIR" worktree add "$WORKTREE_DIR" -b "$BRANCH" "origin/${BASE_BRANCH:-main}"

export AGENTFLEET_BASE_REPO_DIR="$BASE_REPO_DIR"
export AGENTFLEET_WORKTREE_DIR="$WORKTREE_DIR"

cd "$WORKTREE_DIR"
exec node /opt/deepseek-harness/apps/cli/lib/bin.js --profile agentfleet-runner "$TASK"
