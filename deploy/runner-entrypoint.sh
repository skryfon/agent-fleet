#!/usr/bin/env bash
# agentfleet-runner container entrypoint. Clones REPO_URL once into a
# persistent base checkout, carves this Task its own `git worktree` (D2 —
# one worktree per Run is relaxed to one worktree per TASK as of M3: a
# resurrect-and-resume launch gets a brand-new run id but must reattach the
# SAME worktree and dsh session state a prior run in this task left on the
# shared `agentfleet-task-<task-id>` volume, see cmd/supervisor/daemon.go's
# spec()), then execs dsh from inside it — so dsh-fs-local's `cwd` default
# (evaluated once, at module import, before af-worktree's apply() ever runs)
# is already correct. See runner/packages/af-worktree/src/index.ts for why
# worktree CREATION lives here instead of in that plugin.
set -euo pipefail

: "${REPO_URL:?REPO_URL is required, e.g. https://github.com/<owner>/<repo>.git}"
: "${RUN_ID:?RUN_ID is required (a run-unique id, e.g. a uuid)}"
: "${TASK_ID:?TASK_ID is required (the task this run belongs to — names the shared workspace volume/branch)}"
: "${GH_TOKEN:?GH_TOKEN is required by af-github push/gh_pr_create (never baked into the image)}"

# TASK (the prompt) is required for a fresh launch, absent for a resume —
# af-resume reads AF_RESUME_SESSION_ID/AF_RESUME_ANSWER directly from the
# environment instead of a CLI positional (headless-runner, which parses
# TASK, is disabled in that condition — see runner/bundle/cordis.patch.yml).
if [ -z "${AF_RESUME_SESSION_ID:-}" ]; then
  : "${TASK:?TASK is required for a fresh launch (the prompt handed to the headless agent)}"
fi

# `gh` reads GH_TOKEN itself for API calls; this additionally points git's own
# credential helper at it so plain `git clone`/`git push` authenticate too.
gh auth setup-git
git config --global user.name "${AGENTFLEET_GIT_NAME:-agentfleet-runner}"
git config --global user.email "${AGENTFLEET_GIT_EMAIL:-agentfleet-runner@users.noreply.github.com}"

BASE_REPO_DIR="$AGENTFLEET_WORKSPACE_ROOT/base"
WORKTREE_DIR="$AGENTFLEET_WORKSPACE_ROOT/worktree"
BRANCH="agentfleet/$TASK_ID"

if [ -d "$WORKTREE_DIR/.git" ]; then
  # Resume: a prior run in this task already created the worktree on this
  # same task-scoped volume. Reuse it as-is — re-running `worktree add`
  # against an existing path/branch would fail, and the whole point of the
  # shared volume is that nothing here needs redoing.
  git -C "$BASE_REPO_DIR" fetch origin
else
  if [ -d "$BASE_REPO_DIR/.git" ]; then
    git -C "$BASE_REPO_DIR" fetch origin
  else
    git clone "$REPO_URL" "$BASE_REPO_DIR"
  fi

  git -C "$BASE_REPO_DIR" worktree add "$WORKTREE_DIR" -b "$BRANCH" "origin/${BASE_BRANCH:-main}"
fi

export AGENTFLEET_BASE_REPO_DIR="$BASE_REPO_DIR"
export AGENTFLEET_WORKTREE_DIR="$WORKTREE_DIR"

# Relocate DSH_HOME onto the writable task volume (M3 Blocker 1): the image
# bakes the agentfleet-runner profile under /home/agentfleet/.dsh on the
# READ-ONLY rootfs (deploy/runner.Dockerfile) — sessions can't be written
# there at all, let alone survive a checkpoint-and-exit. dsh's own
# resolveDshHome() (deepseek-harness/packages/util/home-paths) reads
# $DSH_HOME from the environment at boot, so moving it here is sufficient;
# no dsh-base config row is patched (D14 does not apply). Seed once — the
# volume persists across every run this task ever launches.
DSH_HOME_VOLUME="$AGENTFLEET_WORKSPACE_ROOT/.dsh"
if [ ! -d "$DSH_HOME_VOLUME" ]; then
  cp -r /home/agentfleet/.dsh "$DSH_HOME_VOLUME"
fi
export DSH_HOME="$DSH_HOME_VOLUME"

cd "$WORKTREE_DIR"

# M6: the manifest-compiled per-role dsh --patch overlay
# (internal/domain/manifest.Manifest.Patch), layered over the profile's own
# runner/bundle/cordis.patch.yml — never a dsh-base row (D14). AF_PATCH is
# unset for a manifest-less project, same as before M6. Written to /tmp
# (tmpfs even under the read-only rootfs, development-plan.md §8) rather
# than passed inline — a multi-line YAML value survives a file, not a CLI
# argument.
patch_args=()
if [ -n "${AF_PATCH:-}" ]; then
  printf '%s' "$AF_PATCH" >/tmp/af-patch.yml
  patch_args=(--patch /tmp/af-patch.yml)
fi

exec node /opt/deepseek-harness/apps/cli/lib/bin.js --profile agentfleet-runner "${patch_args[@]}" "${TASK:-}"
