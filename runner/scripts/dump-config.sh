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
# Usage: runner/scripts/dump-config.sh [--patch <file>] [--check]
#   (no args)     print the current dump-config to stdout
#   --patch FILE  compose with FILE layered on top, the same way
#                 deploy/runner-entrypoint.sh's AF_PATCH does (M6) —
#                 catches a dsh upgrade silently changing how --patch
#                 layers, which plain composition drift alone would miss.
#   --check       diff against the matching golden; exit 1 on drift
#                 (dump-config.golden with no --patch,
#                 dump-config-manifest.golden with one)

set -euo pipefail

orig_pwd="$PWD"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bin="$repo_root/deepseek-harness/apps/cli/lib/bin.js"

patch_file=""
check=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --patch)
      # Resolved against the CALLER's cwd, not repo_root — dsh itself
      # resolves --patch against ITS OWN cwd at boot (target-repo/, once
      # this script cds into its scratch dir below), so a relative path
      # here must become absolute now or dsh would silently look in the
      # wrong place.
      patch_file="$2"
      if [[ "$patch_file" != /* ]]; then
        patch_file="$orig_pwd/$patch_file"
      fi
      shift 2
      ;;
    --check)
      check=1
      shift
      ;;
    *)
      echo "dump-config.sh: unrecognized argument: $1" >&2
      exit 1
      ;;
  esac
done

if [[ -n "$patch_file" ]]; then
  golden="$repo_root/runner/testdata/dump-config-manifest.golden"
else
  golden="$repo_root/runner/testdata/dump-config.golden"
fi

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
# Resolved to its real path (macOS's /var -> /private/var symlink, notably)
# so it matches whatever dsh itself reports after its own path resolution —
# see the output normalization below.
scratch="$(cd "$scratch" && pwd -P)"

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
  "link:$repo_root/runner/packages/af-budget" \
  "link:$repo_root/runner/packages/af-subagent" \
  "link:$repo_root/runner/packages/af-context" \
  "link:$repo_root/runner/packages/af-webhook" >&2

mkdir -p "$scratch/target-repo"

dump_args=(--profile agentfleet-runner --dump-config)
if [[ -n "$patch_file" ]]; then
  # dsh prints the --patch argument VERBATIM in a "patched by <path>"
  # comment in --dump-config's own output — an absolute path here would
  # bake this run's mktemp/repo-checkout location into the golden,
  # breaking --check on every other machine and every CI run. Copying the
  # fixture into target-repo/ under a fixed name and passing a bare
  # relative path keeps that comment (and the golden) identical everywhere.
  cp "$patch_file" "$scratch/target-repo/fixture-manifest.patch.yml"
  dump_args=(--profile agentfleet-runner --patch fixture-manifest.patch.yml --dump-config)
fi

output="$(cd "$scratch/target-repo" && node "$bin" "${dump_args[@]}")"

# dsh resolves --patch to an ABSOLUTE path before printing it in a "patched
# by <path>" comment, regardless of how it was passed — so even the
# relative argument above still bakes this run's mktemp scratch dir into
# the output. Normalize it to a fixed placeholder so the golden is
# identical on every machine and every CI run.
output="${output//$scratch/<SCRATCH>}"

# Every af-* row must be present, and no Cordis fiber may sit PENDING
# (the CLAUDE.md composition smoke test). af-webhook is disabled in the
# bundle patch — deliberately deferred, optional per development-plan.md §7
# M6 — but its package is real enough now (tsconfig, built lib/) to link and
# compose here, catching a load failure even while it stays off.
for id in af-control af-context af-worktree af-github af-policy af-ask-human af-resume af-budget af-subagent af-webhook; do
  grep -q "^- id: $id\$" <<<"$output" || { echo "dump-config.sh: missing af-* row: $id" >&2; exit 1; }
done
grep -q "PENDING" <<<"$output" && { echo "dump-config.sh: a Cordis fiber is PENDING" >&2; exit 1; }

if [[ -n "$check" ]]; then
  printf '%s\n' "$output" > "$scratch/current"
  diff -u "$golden" "$scratch/current" || { echo "dump-config.sh: drift from $golden — update the golden if the diff is benign" >&2; exit 1; }
else
  printf '%s\n' "$output"
fi
