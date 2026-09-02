# dsh upgrade drill (runbook)

Run this whenever the `deepseek-harness` submodule pin moves. First performed as M4.5
(development-plan.md); this file is the checklist for every bump after that one — see
`docs/upgrade-drills/` for past runs and their numbers.

`dsh` is pre-release (`SESSION_FORMAT_VERSION = 0`, no compatibility promise, packages
`private: true`). The point of this drill is not to make the bump succeed at all costs —
it's to measure what it costs, honestly, including "the container build doesn't work in
this environment for reasons unrelated to the bump," if that's what happens.

## Prerequisites

- `git -C deepseek-harness fetch --tags` reaches the upstream repo (HTTPS remote, not
  the default SSH one, unless your machine has a working deploy key for it).
- A branch to bump on, e.g. `git checkout -b dsh-upgrade-<new-tag>` — the bump must be
  revertible in one command.

## Step 1 — land the detectors first, on the OLD pin

Skip this once `runner/scripts/dump-config.sh`, `runner/testdata/dump-config.golden`,
and `runner/packages/af-budget/src/dsh-seam.test.ts` already exist and pass — they do,
as of the M4.5 run. Just confirm they're green before touching the pin:

```sh
./runner/scripts/dump-config.sh --check
cd runner && pnpm run typecheck && npx vitest run
```

If dsh grows a new untyped `ctx.get(...)` seam in an `af-*` package before your next
bump, add a devDependency + type-assignability test for it the same way
`dsh-seam.test.ts` does for `tokenMeter` — see that file's own header comment for the
pattern and why it exists (a duck-typed seam is invisible to `tsc` on the af-* package's
own project, which doesn't import the real dsh package).

## Step 2 — capture the baseline

- Record the old tag, SHA, `deepseek-harness/package.json` version, and
  `SESSION_FORMAT_VERSION` (`grep SESSION_FORMAT_VERSION packages/core/session/src/types.ts`).
- Copy `runner/testdata/dump-config.golden` aside.
- Capture a real session log to resume against later (Step 5). No `OMNI_ROUTE_API_KEY`
  needed — point the profile at dsh's own mock LLM server instead of a real provider:

  ```sh
  # from deepseek-harness/
  node --import tsx packages/test-support/llm-mock-server/src/bin.ts \
    --port 18234 --api-key mock-key --sequence success --repeat-last &

  # a scratch DSH_HOME whose settings.yaml points omni-route's baseURL at
  # http://127.0.0.1:18234/v1 instead of the real OmniRoute — copy
  # deploy/runner-settings.yaml and edit just that one field.
  export DSH_HOME=<scratch>; export OMNI_ROUTE_API_KEY=mock-key
  node deepseek-harness/apps/cli/lib/bin.js plugin --profile agentfleet-runner add \
    "link:$PWD/deepseek-harness/packages/bundle/headless" "link:$PWD/runner/bundle" \
    "link:$PWD/runner/packages/af-policy" "link:$PWD/runner/packages/af-github" \
    "link:$PWD/runner/packages/af-worktree" "link:$PWD/runner/packages/af-control" \
    "link:$PWD/runner/packages/af-ask-human" "link:$PWD/runner/packages/af-resume" \
    "link:$PWD/runner/packages/af-budget"

  cd <scratch-git-repo> && node .../apps/cli/lib/bin.js --profile agentfleet-runner "say hello"
  ```

  Note the session id dsh prints (`$DSH_HOME/sessions/<cwd-slug>/session-<uuid>/`) and
  copy the whole `sessions/` + `storages/` tree aside, untouched, before doing anything
  else with it — resuming it mutates it, and it can't be reconstructed after the bump.
- Confirm `make check` / `go test ./...` is green (it should be — D13 means the Go side
  never touches dsh, so this is a control, not something the bump should move).

## Step 3 — bump and rebuild

```sh
git -C deepseek-harness checkout <new-tag>
cd deepseek-harness && pnpm install --frozen-lockfile && pnpm run build:lib
cd ../runner && pnpm install --frozen-lockfile && pnpm run typecheck && pnpm run build && npx vitest run
```

Log time separately per category — the split matters more than the total:

1. **Toolchain** — `packageManager`/`engines` moved in `deepseek-harness/package.json`;
   `runner/package.json` and `deploy/runner.Dockerfile`'s `node:22.19-...` follow.
2. **Typed drift** — a compile error in `runner/packages/*/src`. Fix and note the file.
3. **Build-script drift** — `build:lib` itself fails or changes shape.
4. **Silent drift** — only Step 4's checks catch this. If nothing here breaks, say so;
   it's real signal, not an empty section.

## Step 4 — composition and live checks

```sh
./runner/scripts/dump-config.sh --check   # every hunk gets triaged, not rubber-stamped
make runner-image                          # builds the real deployed artifact
```

Then, against a real target repo: a full run opens a PR via `af-github`, and
`docs/m4-merge-drill.md` passes end to end (re-run it — its own header says a dsh bump
qualifies as a reason to).

**If `make runner-image` fails:** check whether it also fails on the OLD pin with the
unmodified Dockerfile before attributing the failure to the bump. The M4.5 run hit
exactly this — a Linux/podman-specific `tsdown` build break that reproduced identically
on both pins, i.e. a pre-existing environment issue in that machine's podman setup, not
a dsh regression. Don't let that control step be optional; without it a pre-existing
break gets misattributed to every future bump that happens to run into it.

## Step 5 — the D1 test

Point `af-resume` at Step 2's untouched session copy:

```sh
export AF_RESUME_SESSION_ID=<session id from step 2>
export AF_RESUME_ANSWER="continue"
cd <same scratch git repo> && node .../apps/cli/lib/bin.js --profile agentfleet-runner
```

If it resumes cleanly, `SESSION_FORMAT_VERSION` didn't move (check it directly too —
`grep` the constant). If it doesn't resume, that's the headline finding: decide and
record whether AgentFleet needs a session migration path or can treat in-flight
sessions as disposable across this kind of bump, and write an ADR — it changes M3's
checkpoint-and-exit story.

## Step 6 — write it down

One file per run under `docs/upgrade-drills/<old-tag>-to-<new-tag>.md`: hours by
category, files touched, what `tsc`/`dump-config` caught vs. what only a live run
caught, and anything left unverified with why. Feed the hours into
development-plan.md §10's "dsh upgrade cost" metric — the freeze trigger is two
consecutive upgrades over a week each.

Update `runner/testdata/dump-config.golden` and this repo's dependency pins in the same
PR as the bump. Don't carry a stale golden forward "for next time."
