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

## Status: M1 done

Both M1 done-conditions are proven, live, in a real rootless container
against a real GitHub repo (`sarathkumarps17/agentfleet-m1-scratch`, branch
protection on `main`):

- **Kill criterion** — `af-policy` denies a real tool from a `tools/pre-execute`
  listener, without patching any `dsh-base` config row by id. The profile's
  own `cordis.patch.yml` stayed the untouched `[]` template throughout.
- **First PR** — a real headless task created a branch, wrote a file,
  committed, pushed, and opened
  [PR #1](https://github.com/sarathkumarps17/agentfleet-m1-scratch/pull/1) via
  `af-github`. Wall-clock: 22s. `af-github` has no merge tool (D3); a
  follow-up run's `gh pr merge` attempt was denied by `af-policy`
  (`bash command matched forbidden pattern: gh\s+pr\s+merge`), and a direct
  `git push origin main` was independently rejected server-side by branch
  protection (`GH006: Protected branch update failed`) — two of the four
  merge-prevention layers (development-plan.md §M4), verified.

`af-control` and `af-context` remain comment-only stubs, `disabled: true` in
`bundle/cordis.patch.yml` — they're M2. `af-ask-human`/`af-budget`/
`af-subagent`/`af-webhook` are untouched M3+ stubs.

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

## What's next (M2)

- `af-control`: mirror `session/event` to the control plane; long-poll the
  run inbox.
- `af-context`: resolve hash-pinned `spec_refs` on `agent/pre-step`.
- The fake-LLM runner (dsh already ships `packages/test-support/llm-mock-server`)
  for deterministic, zero-token integration tests of policy/orchestration.
- A hand-launched `podman run` proved the container; M2 adds the supervisor
  service that launches runs for real.

See `~/.claude/plans/objective-implement-the-m1-calm-swing.md` for the full
plan and the OmniRoute wiring writeup.
