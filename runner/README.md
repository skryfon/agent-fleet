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

## Status: M4.5 done

M1's done-conditions (kill criterion + first PR, both proven live against
`sarathkumarps17/agentfleet-m1-scratch`) still hold — see git history for the
original writeup. Since then: M2 (`af-control` mirrors `session/event` to the
control plane), M3 (`af-ask-human`/`af-resume`, checkpoint-and-exit), M4
(`af-policy` four-layer merge prevention incl. the egress proxy, `af-budget`
live with a real cap), and M4.5 (the dsh upgrade drill below) all landed.
Every `af-*` package here is active in `bundle/cordis.patch.yml` except
`af-context` (still a comment-only M2+ stub) and `af-subagent`/`af-webhook`
(M5/M6).

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

## What's next (M5)

- `af-subagent`: `spawn_worker` with depth/fan-out limits, `ask_orchestrator`
  routing, subtree cancellation (development-plan.md §M5).
- `af-context`: resolve hash-pinned `spec_refs` on `agent/pre-step` — still a
  comment-only stub.
- development-plan.md itself notes stop-after-M4.5 is a legitimate outcome;
  M5 is additional orchestration on top of a system that already has real
  safety rails.

See `~/.claude/plans/objective-implement-the-m1-calm-swing.md` for the
original M1 plan and the OmniRoute wiring writeup.
