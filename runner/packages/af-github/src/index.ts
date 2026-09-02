// af-github: branch, commit, push, and open-PR tools via `gh`/`git`. No merge
// tool exists in this package, deliberately (development-plan.md §5, D3).

import { execFile } from 'node:child_process'
import { createHash } from 'node:crypto'
import { promisify } from 'node:util'
import type { Context } from '@deepseek-ai/cordis'
import { defineTool } from '@deepseek-ai/dsh-tools'

const run = promisify(execFile)

export const name = 'af-github'
export const inject = ['tools']

// ponytail: duplicated (not imported) from af-control's own RunClient —
// same reasoning as af-ask-human's own copy (see that package's head
// comment). Only the one method this plugin needs.
interface RunClient {
  dispatchTool(toolName: string, args: unknown): Promise<{ allow: boolean; reason?: string; rule?: string; result?: unknown }>
}

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
    // M4: mediated (development-plan.md §4/§7 — PR creation is exactly the
    // "anything crossing a boundary" this section names) so its policy
    // decision is recorded, AND so the artifact POST /v1/approvals later
    // binds an approval to actually gets written — see internal/api's
    // pr_opened post-allow handler (tools.go).
    async execute(args, exec) {
      const client = ctx.get('afControl') as RunClient | undefined
      if (client === undefined) {
        throw new Error('gh_pr_create: af-control is not configured (RUN_ID/AF_RUN_TOKEN/CONTROL_PLANE_URL unset) — not running under a real AgentFleet run')
      }

      const base = args.base ?? 'main'

      const dispatch = await client.dispatchTool('gh_pr_create', { title: args.title, body: args.body, base })
      if (!dispatch.allow) {
        throw new Error(`gh_pr_create: denied — ${dispatch.reason ?? dispatch.rule ?? 'no reason given'}`)
      }

      const ghArgs = ['pr', 'create', '--title', args.title, '--body', args.body, '--base', base]
      const { stdout } = await run('gh', ghArgs, { signal: exec.signal })
      const url = stdout.trim()

      const { stdout: headSHA } = await run('git', ['rev-parse', 'HEAD'], { signal: exec.signal })
      const { stdout: diff } = await run('git', ['diff', `origin/${base}...HEAD`], { signal: exec.signal, maxBuffer: 64 * 1024 * 1024 })
      const diffSHA256 = createHash('sha256').update(diff).digest('hex')

      const opened = await client.dispatchTool('pr_opened', {
        url, head_sha: headSHA.trim(), diff_sha256: diffSHA256, base,
      })
      if (!opened.allow) {
        // The PR itself already exists (gh_pr_create above succeeded) —
        // this second dispatch only records the artifact an approval binds
        // to. A deny here means REVIEW->DONE has nothing to approve against
        // until a human notices and re-runs it by hand; surfaced as a tool
        // error rather than silently returning success.
        throw new Error(`pr_opened: denied — ${opened.reason ?? opened.rule ?? 'no reason given'}`)
      }

      return url
    },
  }))
}
