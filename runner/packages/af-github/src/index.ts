// af-github: branch, commit, push, and open-PR tools via `gh`/`git`. No merge
// tool exists in this package, deliberately (development-plan.md §5, D3).

import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import type { Context } from '@deepseek-ai/cordis'
import { defineTool } from '@deepseek-ai/dsh-tools'

const run = promisify(execFile)

export const name = 'af-github'
export const inject = ['tools']

// ponytail: every tool here runs `git`/`gh` against process.cwd() (the repo
// af-worktree's entrypoint puts us in). No repo-path config yet — add a
// `cwd` config field if a role ever needs to target a repo outside its own
// worktree.
export function apply(ctx: Context): void {
  ctx.tools.register(defineTool({
    name: 'create_branch',
    description: 'Create and check out a new git branch from the current HEAD.',
    parameters: {
      name: { type: 'string', required: true, description: 'Branch name, e.g. agentfleet/add-widget' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args, exec) {
      if (args.name.trim() === '') throw new Error('create_branch: name must not be empty')
      await run('git', ['checkout', '-b', args.name], { signal: exec.signal })
      return `created and checked out branch ${args.name}`
    },
  }))

  ctx.tools.register(defineTool({
    name: 'commit',
    description: 'Stage all changes and create a git commit.',
    parameters: {
      message: { type: 'string', required: true, description: 'Commit message' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args, exec) {
      if (args.message.trim() === '') throw new Error('commit: message must not be empty')
      await run('git', ['add', '-A'], { signal: exec.signal })
      const { stdout } = await run('git', ['commit', '-m', args.message], { signal: exec.signal })
      return stdout.trim()
    },
  }))

  ctx.tools.register(defineTool({
    name: 'push',
    description: 'Push the current branch to origin, setting upstream tracking.',
    parameters: {},
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(_args, exec) {
      const { stdout: branch } = await run('git', ['rev-parse', '--abbrev-ref', 'HEAD'], { signal: exec.signal })
      const name = branch.trim()
      if (name === 'main' || name === 'master') {
        throw new Error(`push: refusing to push directly on ${name} — create a branch first`)
      }
      await run('git', ['push', '--set-upstream', 'origin', name], { signal: exec.signal })
      return `pushed ${name} to origin`
    },
  }))

  ctx.tools.register(defineTool({
    name: 'gh_pr_create',
    description: 'Open a pull request for the current branch via the gh CLI. No merge tool exists — a human merges by hand.',
    parameters: {
      title: { type: 'string', required: true },
      body: { type: 'string', required: true },
      base: { type: 'string', description: 'Target branch (default: main)' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args, exec) {
      const base = args.base ?? 'main'
      const ghArgs = ['pr', 'create', '--title', args.title, '--body', args.body, '--base', base]
      const { stdout } = await run('gh', ghArgs, { signal: exec.signal })
      return stdout.trim()
    },
  }))
}
