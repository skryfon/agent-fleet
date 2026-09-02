// af-policy: `tools/pre-execute` waterfall denying merge, protected-branch
// push, and out-of-scope writes; emits `policy_violation` (development-plan.md
// §5). Second line of defense after the manifest's tool allow-list.
//
// M1 proof: a real deny from THIS plugin, without patching dsh-base, is the
// kill criterion (development-plan.md M1). Deny lists live on Config, never
// as constants — a role's manifest compiles to this config (M6), not to code.

import type { Context } from '@deepseek-ai/cordis'
import type { PreToolDecision, ToolExecution } from '@deepseek-ai/dsh-tools'

export const name = 'af-policy'
export const inject = ['tools']

// ponytail: duplicated (not imported) from af-control's own RunClient, same
// reasoning as af-ask-human's own copy of this type — see that package's
// head comment. Only the one method this plugin needs.
interface RunClient {
  reportViolation(tool: string, reason: string): Promise<void>
}

export interface Config {
  /** Tool names denied outright (e.g. a merge tool, if one is ever registered). */
  deny: string[]
  /** Regex source strings; a `bash` call whose command matches any is denied. */
  denyBashPatterns: string[]
}

/**
 * M1 default: no schemastery schema yet (that's a manifest-compiler concern,
 * M6). The bundle patch may override `deny`/`denyBashPatterns` outright;
 * an omitted field here falls back to this default, not a hardcoded rule.
 */
const DEFAULT_CONFIG: Config = {
  deny: ['merge', 'gh_pr_merge'],
  denyBashPatterns: ['gh\\s+pr\\s+merge', '\\bgit\\s+push\\b.+\\b(main|master)\\b'],
}

/** Pure predicate so it can be table-tested without booting cordis. */
export function violation(exec: Pick<ToolExecution, 'name' | 'arguments'>, config: Config): string | undefined {
  if (config.deny.includes(exec.name)) {
    return `${exec.name} is not permitted for this role`
  }
  if (exec.name === 'bash') {
    const command = commandOf(exec.arguments)
    if (command !== undefined) {
      for (const source of config.denyBashPatterns) {
        if (new RegExp(source).test(command)) {
          return `bash command matched forbidden pattern: ${source}`
        }
      }
    }
  }
  return undefined
}

function commandOf(args: unknown): string | undefined {
  if (typeof args === 'object' && args !== null && 'command' in args) {
    const command = (args as { command: unknown }).command
    return typeof command === 'string' ? command : undefined
  }
  return undefined
}

export function apply(ctx: Context, config: Partial<Config> = {}): void {
  const resolved: Config = { ...DEFAULT_CONFIG, ...config }

  ctx.on('tools/pre-execute', async (exec, next): Promise<PreToolDecision> => {
    const reason = violation(exec, resolved)
    if (reason === undefined) return next() // not our call to make — delegate
    ctx.logger.warn(`policy_violation tool=${exec.name} reason=${reason}`)

    // Fire-and-forget: this deny already happened (the tool is being
    // blocked either way) — a report that fails to reach the control plane
    // must not turn a correctly-enforced deny into a hung tool call.
    const client = ctx.get('afControl') as RunClient | undefined
    void client?.reportViolation(exec.name, reason).catch((error: unknown) => {
      ctx.logger.warn(`af-policy: reporting violation failed: ${String(error)}`)
    })

    return { kind: 'deny', reason }
  })

  // Monotonic second layer: nothing downstream can undo this once registered,
  // even a later pre-execute listener that returns 'allow'.
  ctx.effect(() => ctx.tools.guard(exec => violation(exec, resolved)))
}
