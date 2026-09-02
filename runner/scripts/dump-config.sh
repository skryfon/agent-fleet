#!/usr/bin/env bash
# Composes the agentfleet-runner profile and prints its config tree.
#
# Used by M4.5's upgrade drill (development-plan.md) and, from here on, by
# CI: a diff against runner/testdata/dump-config.golden catches dsh
# composition drift (a renamed dsh-base row id, a patched row disappearing,
# an af-* package failing to mount) that `pnpm run build` alone would miss.
#
# --dump-config composes the tree WITHOUT booting fibers or evaluating `!!js`
# expressions, so AF_RESUME_SESSION_ID does not change this output — the
# af-resume/headless-startup/headless-runner `disabled:` lines are printed as
# their unevaluated source, identically, whichever boot path runs for real.
# One snapshot covers both paths; don't run this twice per env var state.
#
# ponytail: never `pnpm dsh` — see runner/README.md and CLAUDE.md, cwd-pinned
# pnpm scripts silently break tools that shell out relative to invoking dir.
#
# Usage: runner/scripts/dump-config.sh [--check]
#   (no args)  print the current dump-config to stdout
#   --check    diff against testdata/dump-config.golden; exit 1 on drift

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="$repo_root/deepseek-harness/apps/cli/lib/bin.js"
golden="$repo_root/runner/testdata/dump-config.golden"

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

export DSH_HOME="$scratch/dsh-home"
mkdir -p "$DSH_HOME"

node "$bin" plugin --profile agentfleet-runner add \
  "link:$repo_root/deepseek-harness/packages/bundle/headless" \
  "link:$repo_root/runner/bundle" \
  "link:$repo_root/runner/packages/af-policy" \
  "link:$repo_root/runner/packages/af-github" \
  "link:$repo_root/runner/packages/af-worktree" \
  "link:$repo_root/runner/packages/af-control" \
  "link:$repo_root/runner/packages/af-ask-human" \
  "link:$repo_root/runner/packages/af-resume" \
  "link:$repo_root/runner/packages/af-budget" >&2

mkdir -p "$scratch/target-repo"
output="$(cd "$scratch/target-repo" && node "$bin" --profile agentfleet-runner --dump-config)"

# Every af-* row must be present, and no Cordis fiber may sit PENDING
# (the CLAUDE.md composition smoke test).
for id in af-control af-context af-worktree af-github af-policy af-ask-human af-resume af-budget af-subagent af-webhook; do
  grep -q "^- id: $id\$" <<<"$output" || { echo "dump-config.sh: missing af-* row: $id" >&2; exit 1; }
done
grep -q "PENDING" <<<"$output" && { echo "dump-config.sh: a Cordis fiber is PENDING" >&2; exit 1; }

if [[ "${1:-}" == "--check" ]]; then
  printf '%s\n' "$output" > "$scratch/current"
  diff -u "$golden" "$scratch/current" || { echo "dump-config.sh: drift from $golden — update the golden if the diff is benign" >&2; exit 1; }
else
  printf '%s\n' "$output"
fi
