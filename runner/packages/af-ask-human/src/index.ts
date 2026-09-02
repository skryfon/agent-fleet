// af-ask-human: registers `ask_human` / `ask_orchestrator` and drives the
// checkpoint-and-exit half of the M3 round trip (development-plan.md §5,
// §6). Resuming and injecting the answer back into the model is af-resume's
// job, not this package's — see runner/packages/af-resume.
//
// ponytail: reads `afControl` (runner/packages/af-control) via ctx.get at
// call time, not Cordis `inject` — af-control no-ops outside a real run
// (RUN_ID/AF_RUN_TOKEN unset), and a hard inject dependency on a service
// that legitimately never provides would leave this plugin's own fiber
// PENDING forever in --dump-config. A missing client is a clear runtime
// error inside execute(), not a boot-time hang.
//
// ponytail: RunClient is duplicated (not imported) from af-control's own
// module — `tsc -b --noEmit` (runner's `typecheck` script) errors on any
// intra-workspace project reference that also emits declarations, and
// af-control's built lib/ isn't guaranteed to exist ahead of a bare
// typecheck run. A tiny shape duplication is cheaper than teaching the
// build graph about a cross-package dependency neither package structurally
// needs (both only exchange plain JSON over ctx.get('afControl')).

import type { Context } from '@deepseek-ai/cordis'
import { defineTool } from '@deepseek-ai/dsh-tools'

export const name = 'af-ask-human'
export const inject = ['tools']

export interface RunClient {
  dispatchTool(toolName: string, args: unknown): Promise<{ allow: boolean; reason?: string; rule?: string; result?: unknown }>
  checkpoint(dshSessionID: string | undefined): Promise<void>
  pollInboxOnce(waitSeconds: number): Promise<{ kind: string; question_id?: string; answer?: string } | undefined>
}

// development-plan.md §6: "The runner polls the inbox for five minutes,
// then checkpoints and exits the container." GET /v1/runs/{id}/inbox caps
// one long-poll at 60s (internal/api/inbox.go), so this is a reconnect
// loop, not one request.
const POLL_WAIT_SECONDS = 55
const TOTAL_WAIT_MS = 5 * 60 * 1000

export interface Config {
  pollWaitSeconds?: number
  totalWaitMs?: number
}

const askHumanParams = {
  question: { type: 'string', required: true, description: 'The question to ask a human.' },
  kind: { type: 'string', required: true, description: "One of 'choice', 'confirm', 'free_text'." },
  options: { type: 'array', items: { type: 'string' }, description: 'Choices, for kind=choice.' },
  addressee: { type: 'string', description: 'Role to address (e.g. architect); omit for the default.' },
} as const

// Exported for testing only — apply() is the package's real entry point.
export async function waitForAnswer(
  client: RunClient, questionID: string, pollWaitSeconds: number, totalWaitMs: number, signal: AbortSignal,
): Promise<string | undefined> {
  const deadline = Date.now() + totalWaitMs

  while (Date.now() < deadline) {
    if (signal.aborted) return undefined

    const msg = await client.pollInboxOnce(pollWaitSeconds)
    if (msg?.kind === 'answer' && msg.question_id === questionID) return msg.answer
  }

  return undefined
}

/** Thrown when ask_human's 5-minute wait elapses with no answer — the
 * caller (this file's own turn-stopping listener) checkpoints and exits
 * rather than surfacing this as an ordinary tool error to the model. */
class AskHumanPending extends Error {
  constructor(readonly questionID: string) {
    super(`ask_human: no answer within the wait budget — checkpointing and exiting (question ${questionID})`)
  }
}

export function apply(ctx: Context, config: Config = {}): void {
  const pollWaitSeconds = config.pollWaitSeconds ?? POLL_WAIT_SECONDS
  const totalWaitMs = config.totalWaitMs ?? TOTAL_WAIT_MS

  let pending: AskHumanPending | undefined

  // toolName is dispatched VERBATIM to POST /v1/runs/{id}/tools/{toolName} —
  // this is what makes D7 (docs/adr/0007: "only the orchestrator gets
  // ask_human") enforceable at all. Both callers below used to hardcode
  // 'ask_human' here regardless of which tool actually invoked ask(), so a
  // worker calling ask_orchestrator was evaluated by internal/policy as
  // 'ask_human' and denied — a real bug caught while wiring M5's D7
  // enforcement test, fixed by threading the real tool name through.
  async function ask(toolName: 'ask_human' | 'ask_orchestrator', args: { question: string; kind: string; options?: string[]; addressee?: string }, signal: AbortSignal): Promise<string> {
    const client = ctx.get('afControl') as RunClient | undefined
    if (client === undefined) {
      throw new Error(`${toolName}: af-control is not configured (RUN_ID/AF_RUN_TOKEN/CONTROL_PLANE_URL unset) — not running under a real AgentFleet run`)
    }

    const dispatch = await client.dispatchTool(toolName, {
      question: args.question, kind: args.kind, options: args.options, addressee: args.addressee,
    })
    if (!dispatch.allow) {
      throw new Error(`${toolName}: denied — ${dispatch.reason ?? dispatch.rule ?? 'no reason given'}`)
    }

    const questionID = (dispatch.result as { question_id?: string } | undefined)?.question_id
    if (questionID === undefined) {
      throw new Error(`${toolName}: control plane accepted the question but returned no question_id`)
    }

    const answer = await waitForAnswer(client, questionID, pollWaitSeconds, totalWaitMs, signal)
    if (answer !== undefined) return answer

    pending = new AskHumanPending(questionID)
    throw pending
  }

  ctx.tools.register(defineTool({
    name: 'ask_human',
    description: 'Ask a human a question and unblock only once they answer. May pause the run for hours — use for genuine ambiguity, not busywork.',
    parameters: askHumanParams,
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args, exec) { return ask('ask_human', args, exec.signal) },
  }))

  // ask_orchestrator (D7, M5): same mechanics, dispatched under its own
  // tool name (see ask()'s own doc comment above) so internal/policy
  // evaluates a worker's manifest role against ask_orchestrator, never
  // ask_human — the manifest allow-list is what actually gates who can
  // reach either tool (development-plan.md §5: "allow-list first").
  ctx.tools.register(defineTool({
    name: 'ask_orchestrator',
    description: 'Ask the coordinating orchestrator a question (workers only — D7).',
    parameters: askHumanParams,
    output: { schema: { type: 'string' }, render: (_args, value) => [{ type: 'text', text: value }] },
    async execute(args, exec) { return ask('ask_orchestrator', args, exec.signal) },
  }))

  // The checkpoint-and-exit half: a turn that stopped because ask_human's
  // wait budget elapsed (not answered, not denied, not any other tool
  // error) reports the run's dsh session id and requests process exit —
  // development-plan.md §6's "checkpoints and exits the container." The
  // `question` row already IS the durable state (Store.ApplyAsk wrote it
  // before this plugin ever started waiting), so nothing else needs saving.
  ctx.effect(() => ctx.on('agent/turn-stopping', async ({ agent }) => {
    if (pending === undefined) return

    const client = ctx.get('afControl') as RunClient | undefined
    const sessions = ctx.get('sessions')
    const exit = ctx.get('appExit')

    if (client !== undefined) {
      await client.checkpoint(agent.session.id).catch((error: unknown) => {
        ctx.logger.warn(`af-ask-human: checkpoint failed: ${String(error)}`)
      })
    }

    await sessions?.flush(agent.session)

    // ponytail: exits immediately after the checkpoint call succeeds or
    // fails — no retry budget on the checkpoint POST itself. A dropped
    // checkpoint just means af-resume's AF_RESUME_SESSION_ID falls back to
    // this run's own dsh_session_id column staying unset, which resume
    // launch (internal/supervisor.RunLaunch) already treats as "start a
    // fresh session" rather than crashing — acceptable degradation, not a
    // silent data loss.
    exit?.(0)
  }))
}
