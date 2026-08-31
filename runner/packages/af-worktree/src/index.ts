// af-worktree: confines a run to its own `git worktree` and tears it down on
// exit (development-plan.md §5, D2: one container per Run, one worktree per
// Run).
//
// ponytail: worktree CREATION happens in the container entrypoint
// (deploy/runner-entrypoint.sh), not here. `dsh-fs-local`'s Config resolves
// its `cwd` default from `process.cwd()` at plugin-module-import time, which
// races any `process.chdir()` this plugin could do inside `apply()` — the
// entrypoint `cd`s into the worktree in the shell, before `node` even
// starts, which has no such race. This plugin owns only the half a shell
// script can't: registering `git worktree remove` as a disposal effect, so
// it runs when the cordis tree tears down (dsh-headless disposes the tree
// before requesting process exit). If M2 needs worktree creation to be
// agent-visible (multi-repo tasks, a `spawn_worker` child choosing its own
// branch), add a `prepare_worktree` tool here instead of pushing this logic
// back into the entrypoint.

import { execFileSync } from 'node:child_process'
import type { Context } from '@deepseek-ai/cordis'

export const name = 'af-worktree'

export interface Config {
  /** The persistent base clone `git worktree add` was run against. */
  baseRepoDir?: string
  /** This run's worktree directory (also the process cwd, set by the entrypoint). */
  worktreeDir?: string
}

export function apply(ctx: Context, config: Config = {}): void {
  if (config.baseRepoDir === undefined || config.worktreeDir === undefined) return

  const { baseRepoDir, worktreeDir } = config
  ctx.effect(() => () => {
    try {
      execFileSync('git', ['-C', baseRepoDir, 'worktree', 'remove', '--force', worktreeDir])
    } catch (error) {
      ctx.logger.warn(`af-worktree: teardown of ${worktreeDir} failed: ${String(error)}`)
    }
  })
}
