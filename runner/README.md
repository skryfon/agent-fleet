# runner/

The `agentfleet-runner` dsh profile: `dsh-base` + `dsh-headless` +
`dsh-bundle-agentfleet` (development-plan.md §5). Pnpm workspace of Cordis
plugins that get bundled into that profile — see `.claude/skills/dsh-plugin-dev`
before editing any `af-*` package.

## Layout

- `packages/af-*` — one Cordis plugin per use case (see each `src/index.ts`
  for its seam and milestone).
- `bundle/` — `dsh-bundle-agentfleet`, the patch that stacks the `af-*`
  plugins into a profile.

## Status: M0–M6 done

M1's done-conditions (kill criterion + first PR, both proven live against
`sarathkumarps17/agentfleet-m1-scratch`) still hold — see git history for the
original writeup. Since then: M2 (`af-control` mirrors `session/event` to the
control plane), M3 (`af-ask-human`/`af-resume`, checkpoint-and-exit), M4
(`af-policy` four-layer merge prevention incl. the egress proxy, `af-budget`
live with a real usd/minutes/questions cap), M4.5 (the dsh upgrade drill
below), M5 (`af-subagent`'s `spawn_worker`/`ask_orchestrator`/subtree
cancellation), and M6 (the manifest compiler, `af-context`'s D8 hash-pinned
`spec_refs` enforcement) all landed. Every `af-*` package here is active in
`bundle/cordis.patch.yml` except `af-webhook`, which stays `disabled: true`
— development-plan.md §7 calls it optional for M6, so M6 is complete without
it.

`af-context` (`packages/af-context/src/index.ts`) verifies each task's
`spec_refs` — `{path, anchor, sha256}`, carried from the task row to the
container as `AF_SPEC_REFS` — against the worktree on `agent/pre-step`, and
rejects the step on a mismatch. An anchor's semantics aren't specified by the
plan; this package treats it as a markdown heading slug (GitHub's own
heading-to-slug rule) and hashes from that heading through the line before
the next heading of equal-or-higher level. No anchor hashes the whole file.
No `AF_SPEC_REFS`/`AGENTFLEET_WORKTREE_DIR` (a bare host run, or a task with
no spec_refs) is a no-op, same fallback shape as `af-worktree`'s own config.

### Upgrading dsh

See `docs/dsh-upgrade-drill.md` for the runbook and `docs/upgrade-drills/` for
past runs. Two regression detectors exist specifically for this:
`scripts/dump-config.sh` (composition drift — every `af-*` row present, no
`PENDING` Cordis fiber, diffed against `testdata/dump-config.golden`) and
`packages/af-budget/src/dsh-seam.test.ts` (a type-only check that af-budget's
duck-typed `tokenMeter` seam still matches the real
`@deepseek-ai/dsh-token-meter` contract — that package is deliberately not
imported at runtime, see the file's own header, so `tsc` on af-budget's own
project can't catch drift there without this).

### Commands

```sh
pnpm install               # this workspace
pnpm run typecheck         # tsc -b --noEmit, project-referenced
pnpm run build             # tsc -b -> packages/*/lib
npx vitest run             # unit tests (packages/*/src/**/*.test.ts only)
```

### The container

```sh
# from the repo root
podman build -f deploy/runner.Dockerfile -t agentfleet-runner .
podman run --rm \
  -e OMNI_ROUTE_API_KEY -e GH_TOKEN \
  -e REPO_URL=https://github.com/<owner>/<repo>.git \
  -e RUN_ID=<run-unique-id> \
  -e TASK="..." \
  agentfleet-runner
```

The build stage runs deepseek-harness's **`pnpm run build:lib`**, not
`build:lib:host` alone — the host-only build silently leaves some packages
(e.g. `dsh-typert-registry`) without their `lib/index.js`, which only
surfaces as a `ERR_MODULE_NOT_FOUND` at full boot, not at `--dump-config`
(dump-config composes the tree without booting fibers). `build:web` (the
browser frontend) is skipped — irrelevant to a headless-only profile. See
`apps/cli/README.md`'s Development section for the documented production
build.

The entrypoint (`deploy/runner-entrypoint.sh`) clones `REPO_URL` into a
persistent base checkout and carves this Run its own `git worktree` (D2)
*before* `node` starts — see `packages/af-worktree/src/index.ts` for why
worktree creation lives there instead of in that plugin (a `process.chdir()`
inside `apply()` would race `dsh-fs-local`'s `cwd` default, which resolves
once at plugin-module-import time).

### Running the profile locally (dev, not the container)

The profile lives under `$DSH_HOME/profiles/agentfleet-runner` (created via
`dsh plugin --profile agentfleet-runner add <bundle/plugin specs>`, see
`deepseek-harness/apps/cli/README.md`). Invoke the **built** CLI directly —
`pnpm dsh` inside `deepseek-harness/` runs its script with cwd pinned to that
package root, which silently breaks any tool that shells out relative to the
invoking directory (e.g. `create_branch`):

```sh
cd <target-repo>   # the workspace root dsh operates on
node ../deepseek-harness/apps/cli/lib/bin.js --profile agentfleet-runner --dump-config
node ../deepseek-harness/apps/cli/lib/bin.js --profile agentfleet-runner "<task>"
```

`apps/cli/lib/bin.js` is dsh's own already-built artifact in the vendored
checkout — no `pnpm run build` needed there.

## What's next (M7/M8)

- `af-webhook`: auto-start a read-only review session on GitHub
  `ready_for_review`, modeled on dsh's own `github-review` example —
  optional per development-plan.md §7 M6, still a comment-only stub,
  packaged (tsconfig, real `lib/` build) but disabled.
- M7's web app (`webapp/`) is built, but its done-condition — daily use —
  hasn't happened yet.
- M8 (planning ingestion): development-plan.md says not to start until
  M1–M7 have been in daily use for a month.

See `~/.claude/plans/objective-implement-the-m1-calm-swing.md` for the
original M1 plan and the OmniRoute wiring writeup.
