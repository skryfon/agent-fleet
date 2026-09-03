// af-subagent: `spawn_worker`/`report_to_orchestrator`/`check_workers`/
// `report_deviation`, all routed through the control plane
// (development-plan.md §5/§7 M5). Every child a worker needs — a Run row,
// a budget, its own PR — is created via POST /v1/runs/{id}/tools/spawn_worker,
// never an in-process dsh subagent: an in-process child is invisible to the
// control plane (development-plan.md §5: "Implementation subagents must be
// spawned, not in-process").

import type { Context } from '@deepseek-ai/cordis'
import { defineTool } from '@deepseek-ai/dsh-tools'

export const name = 'af-subagent'
export const inject = ['tools']

// ponytail: duplicated (not imported) from af-control's own RunClient —
// same reasoning as af-ask-human's/af-github's own copies (see
// af-ask-human's head comment): tsc -b --noEmit errors on an
// intra-workspace project reference that also emits declarations, and
// af-control's built lib/ isn't guaranteed to exist ahead of a bare
// typecheck run. Only the fields this plugin actually reads.
interface RunClient {
  dispatchTool(toolName: string, args: unknown): Promise<{ allow: boolean; reason?: string; rule?: string; result?: unknown }>
  pollInboxOnce(waitSeconds: number): Promise<{ kind: string; payload?: unknown } | undefined>
}

function client(ctx: Context): RunClient {
  const c = ctx.get('afControl') as RunClient | undefined
  if (c === undefined) {
    throw new Error('af-subagent: af-control is not configured (RUN_ID/AF_RUN_TOKEN/CONTROL_PLANE_URL unset) — not running under a real AgentFleet run')
  }

  return c
}

const CHECK_WORKERS_WAIT_SECONDS = 5

export interface Config {
  /**
   * Role names this role's `spawn_worker` may target — M6's manifest
   * `agents.<role>.subagents.spawned`
   * (internal/domain/manifest.Manifest.Patch). Undefined/empty means
   * unrestricted (every deployment before M6, and any role a manifest
   * doesn't mention), matching af-policy's own "omitted config key keeps
   * the default" convention. This is a LOCAL, fail-fast check — the
   * control plane's own tool-dispatch policy (internal/policy.Evaluate)
   * still gates the mediated spawn_worker call regardless; this just saves
   * a round trip on an obviously-out-of-scope role.
   */
  spawnedRoles?: string[]
}

/**
 * Pure predicate so it can be table-tested without booting cordis — same
 * pattern as af-policy's own `violation()`. Returns an error message, or
 * undefined to allow.
 */
export function spawnRoleViolation(role: string | undefined, spawnedRoles: string[] | undefined): string | undefined {
  if (spawnedRoles === undefined || spawnedRoles.length === 0) return undefined
  if (role === undefined) return undefined
  if (spawnedRoles.includes(role)) return undefined

  return `spawn_worker: role ${role} is not in this role's manifest subagents.spawned list (${spawnedRoles.join(', ')})`
}

export function apply(ctx: Context, config: Config = {}): void {
  ctx.tools.register(defineTool({
    name: 'spawn_worker',
    description: 'Spawn a worker to implement a piece of work as its own task, tracked and budgeted independently. Subject to the deployment\'s depth and fan-out caps.',
    parameters: {
      title: { type: 'string', required: true, description: 'Short task title.' },
      intent: { type: 'string', required: true, description: 'What the worker should accomplish.' },
      acceptance_criteria: { type: 'array', items: { type: 'string' }, description: 'Done-when criteria, one per line.' },
      role: { type: 'string', description: 'Role override (default: the deployment\'s default role).' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      const violation = spawnRoleViolation(args.role, config.spawnedRoles)
      if (violation !== undefined) {
        throw new Error(violation)
      }

      const dispatch = await client(ctx).dispatchTool('spawn_worker', {
        title: args.title, intent: args.intent, acceptance_criteria: args.acceptance_criteria, role: args.role,
      })
      if (!dispatch.allow) {
        throw new Error(`spawn_worker: denied — ${dispatch.reason ?? dispatch.rule ?? 'no reason given'}`)
      }

      const taskID = (dispatch.result as { task_id?: string } | undefined)?.task_id
      if (taskID === undefined) {
        throw new Error('spawn_worker: control plane accepted the spawn but returned no task_id')
      }

      return `spawned worker task ${taskID}`
    },
  }))

  ctx.tools.register(defineTool({
    name: 'report_to_orchestrator',
    description: 'Report progress or a result to the coordinating orchestrator (workers only — D7). Does not block.',
    parameters: {
      summary: { type: 'string', required: true, description: 'What happened.' },
      status: { type: 'string', required: true, description: "e.g. 'progress', 'done', 'stuck'." },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      const dispatch = await client(ctx).dispatchTool('report_to_orchestrator', { summary: args.summary, status: args.status })
      if (!dispatch.allow) {
        throw new Error(`report_to_orchestrator: denied — ${dispatch.reason ?? dispatch.rule ?? 'no reason given'}`)
      }

      return 'reported to orchestrator'
    },
  }))

  ctx.tools.register(defineTool({
    name: 'check_workers',
    description: 'Check for pending questions or reports from spawned workers (orchestrator only). Returns immediately if nothing is pending yet within the wait window.',
    parameters: {
      wait_seconds: { type: 'number', description: `How long to wait for something to arrive (default ${CHECK_WORKERS_WAIT_SECONDS}, max 60).` },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      const msg = await client(ctx).pollInboxOnce(args.wait_seconds ?? CHECK_WORKERS_WAIT_SECONDS)
      if (msg === undefined || (msg.kind !== 'worker_question' && msg.kind !== 'worker_report')) {
        return 'no pending worker questions or reports'
      }

      return `${msg.kind}: ${JSON.stringify(msg.payload ?? {})}`
    },
  }))

  ctx.tools.register(defineTool({
    name: 'report_deviation',
    description: 'Record a deviation from the assigned spec/intent, with the reason — feeds the drift-rate metric (development-plan.md §11). Use when you had to make a real judgment call the spec did not cover, not for routine implementation choices.',
    parameters: {
      what: { type: 'string', required: true, description: 'What you did differently.' },
      why: { type: 'string', required: true, description: 'Why the spec/intent did not cover this.' },
    },
    output: {
      schema: { type: 'string' },
      render: (_args, value) => [{ type: 'text', text: value }],
    },
    async execute(args) {
      const dispatch = await client(ctx).dispatchTool('report_deviation', { what: args.what, why: args.why })
      if (!dispatch.allow) {
        throw new Error(`report_deviation: denied — ${dispatch.reason ?? dispatch.rule ?? 'no reason given'}`)
      }

      return 'deviation recorded'
    },
  }))
}
