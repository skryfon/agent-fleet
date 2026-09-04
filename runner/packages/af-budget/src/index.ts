// af-budget: reads the token meter on `agent/pre-step` and rejects the step
// on cost/minute cap breach (development-plan.md §5, §6, M4). The control
// plane is the authority on caps and running spend (internal/api's usage
// handler, internal/store.RecordUsage) — this plugin only measures its own
// session and reports deltas; POST /v1/runs/{id}/usage's response tells it
// whether THIS run just breached, and that's what stops the step.
//
// ponytail: enforced only at `agent/pre-step` (a real `{kind:'reject'}` from
// this seam, per dsh's own agent-lifecycle.md, stops the step from
// entering) — not also at `agent/turn-stopping`. A rejected step already
// ends the turn one step earlier than turn-stopping would fire anyway, so a
// second listener on that seam would be pure redundancy for the same
// signal, not a stronger guarantee.
//
// ponytail: reads `afControl`/`tokenMeter` via ctx.get at call time, not
// Cordis `inject` — both no-op outside a real run (af-control) or when the
// composition doesn't mount dsh-token-meter, and either as a hard inject
// dependency would leave this plugin's own fiber PENDING in --dump-config
// (same reasoning as af-ask-human's own head comment).
//
// ponytail: flat $/Mtok pricing from config, not per-route pricing
// (@deepseek-ai/dsh-token-meter/route-pricing) — swap when a deployment
// mixes model families enough for that to matter.

import type { Context } from '@deepseek-ai/cordis'
import type { PreStepDecision } from '@deepseek-ai/dsh-agent'

export const name = 'af-budget'

// Duplicated (not imported) from af-control's own RunClient — see
// af-ask-human's head comment for why.
interface RunClient {
  postUsage(delta: { tokens_in: number; tokens_out: number; cost_usd: number; minutes: number }): Promise<{ breached: boolean; kind?: string }>
}

// Duck-typed against @deepseek-ai/dsh-token-meter's TokenMeter service —
// not imported, so this package has no build-graph dependency on it (it is
// an optional composition mount; see this file's own head comment).
interface TokenMeterService {
  measure(session: unknown): { totalTokens: number }
}

export interface Config {
  /** $ per million total (input+output) tokens — a flat rate, see this file's head comment. */
  usdPerMillionTokens?: number
  /** How often a step's usage is reported, minimum. Default 60s — no reason to hit the control plane every single step. */
  reportIntervalMs?: number
}

const DEFAULT_USD_PER_MILLION_TOKENS = 3
const DEFAULT_REPORT_INTERVAL_MS = 60_000

// Pure so a config-plumbing regression (e.g. usdPerMillionTokens arriving as
// an object instead of a number — see runner/bundle/cordis.patch.yml's head
// comment on its own past bug) shows up as a failing assertion here instead
// of a silently-never-firing USD cap.
export function costUSD(deltaTokens: number, usdPerMillionTokens: number): number {
  return (deltaTokens / 1_000_000) * usdPerMillionTokens
}

export function apply(ctx: Context, config: Config = {}): void {
  const usdPerMillionTokens = config.usdPerMillionTokens ?? DEFAULT_USD_PER_MILLION_TOKENS
  const reportIntervalMs = config.reportIntervalMs ?? DEFAULT_REPORT_INTERVAL_MS

  const startedAt = Date.now()
  let lastReportedTokens = 0
  let lastReportedAt = 0
  let breached: string | undefined

  ctx.on('agent/pre-step', async ({ agent, signal }, next): Promise<PreStepDecision> => {
    if (breached !== undefined) return { kind: 'reject' }

    const decision = await next()
    if (decision.kind === 'reject' || signal.aborted) return decision

    const client = ctx.get('afControl') as RunClient | undefined
    const tokenMeter = ctx.get('tokenMeter') as TokenMeterService | undefined
    if (client === undefined || tokenMeter === undefined) return decision

    const now = Date.now()
    if (now - lastReportedAt < reportIntervalMs) return decision

    const { totalTokens } = tokenMeter.measure(agent.session)
    const deltaTokens = Math.max(0, totalTokens - lastReportedTokens)
    const minutes = Math.floor((now - startedAt) / 60_000)

    lastReportedTokens = totalTokens
    lastReportedAt = now

    try {
      const result = await client.postUsage({
        tokens_in: deltaTokens, tokens_out: 0,
        cost_usd: costUSD(deltaTokens, usdPerMillionTokens),
        minutes,
      })

      if (result.breached) {
        breached = result.kind ?? 'unknown'
        ctx.logger.warn(`af-budget: breached (${breached}) — rejecting further steps`)

        return { kind: 'reject' }
      }
    } catch (error) {
      // A failed report is not itself a breach — af-budget degrades to "no
      // enforcement this step," not "stop the run," since the control
      // plane's own usage handler is the authoritative stop either way
      // (internal/api's recordUsage cancels the task independently on a
      // breach it observes, so a network blip here doesn't undermine M4's
      // hard-kill guarantee).
      ctx.logger.warn(`af-budget: reporting usage failed: ${String(error)}`)
    }

    return decision
  })
}
