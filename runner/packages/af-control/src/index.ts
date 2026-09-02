// af-control: mirrors dsh `session/event` to the control plane and reports
// the run's own dsh session id at checkpoint time (development-plan.md §5,
// §4). M3's other runner-facing concern — af-ask-human's inbox long-poll —
// lives in that package, not here: this plugin owns the outbound mirror and
// the checkpoint call, not a second competing inbox client.
//
// ponytail: batches on a fixed interval, never on backpressure/size; no
// cancel-delivery consumer yet (the inbox's "cancel" kind has existed since
// M2 with no reader — a real consumer is a small addition once a cancel
// needs testing, not required for M3's ask/exit/resume path).

import type { Context } from '@deepseek-ai/cordis'
import type { SessionEvent } from '@deepseek-ai/dsh-session'

export const name = 'af-control'

export interface Config {
  /** POST /v1/runs/{id}/events target base, e.g. http://control-plane:8080. */
  controlPlaneURL?: string
  runID?: string
  /** Plaintext per-run bearer token (AF_RUN_TOKEN) — never logged. */
  runToken?: string
  /** How often buffered session/event rows flush. Default 3000ms. */
  flushIntervalMs?: number
}

const DEFAULT_FLUSH_INTERVAL_MS = 3000

interface MirrorEvent {
  seq: number
  kind: string
  actor: string
  payload: unknown
  at: string
}

/** Shared by af-ask-human/af-policy/af-budget — kept here since af-control owns run/token/URL config. */
export interface RunClient {
  postEvents(events: MirrorEvent[]): Promise<void>
  checkpoint(dshSessionID: string | undefined): Promise<void>
  dispatchTool(toolName: string, args: unknown): Promise<{ allow: boolean; reason?: string; rule?: string; result?: unknown }>
  /**
   * kind is 'cancel' (M2, derived from run.state), 'answer' (M3's
   * ask_human/ask_orchestrator round trip), or — M5 — 'worker_question' /
   * 'worker_report' (af-subagent's check_workers reads these off payload).
   */
  pollInboxOnce(waitSeconds: number): Promise<{ kind: string; question_id?: string; answer?: string; payload?: unknown; from_run_id?: string } | undefined>
  /** POST /v1/runs/{id}/violations (M4) — af-policy's own runner-side deny, distinct from a mediated tool-dispatch deny (which internal/api records itself). */
  reportViolation(tool: string, reason: string): Promise<void>
  /** POST /v1/runs/{id}/usage (M4) — af-budget's periodic report; the response says whether this run just breached its cap. */
  postUsage(delta: { tokens_in: number; tokens_out: number; cost_usd: number; minutes: number }): Promise<{ breached: boolean; kind?: string }>
}

function buildClient(controlPlaneURL: string, runID: string, runToken: string): RunClient {
  const base = `${controlPlaneURL}/v1/runs/${runID}`
  const headers = { Authorization: `Bearer ${runToken}`, 'Content-Type': 'application/json' }

  return {
    async postEvents(events) {
      if (events.length === 0) return
      await fetch(`${base}/events`, { method: 'POST', headers, body: JSON.stringify({ events }) })
    },
    async checkpoint(dshSessionID) {
      await fetch(`${base}/checkpoint`, {
        method: 'POST', headers, body: JSON.stringify({ dsh_session_id: dshSessionID }),
      })
    },
    async dispatchTool(toolName, args) {
      const res = await fetch(`${base}/tools/${toolName}`, { method: 'POST', headers, body: JSON.stringify(args) })
      return res.json() as Promise<{ allow: boolean; reason?: string; rule?: string; result?: unknown }>
    },
    async pollInboxOnce(waitSeconds) {
      const res = await fetch(`${base}/inbox?wait=${waitSeconds}`, { headers })
      if (res.status === 204) return undefined

      return res.json() as Promise<{ kind: string; question_id?: string; answer?: string }>
    },
    async reportViolation(tool, reason) {
      await fetch(`${base}/violations`, { method: 'POST', headers, body: JSON.stringify({ tool, reason }) })
    },
    async postUsage(delta) {
      const res = await fetch(`${base}/usage`, { method: 'POST', headers, body: JSON.stringify(delta) })
      return res.json() as Promise<{ breached: boolean; kind?: string }>
    },
  }
}

// asMirrorEvent maps a dsh SessionEvent onto the control plane's own
// mirror-batch shape (internal/api's postRunEventsRequest) — seq/kind/actor/
// payload/at, the fields internal/store.AppendMirror actually persists.
function asMirrorEvent(ev: SessionEvent): MirrorEvent {
  return { seq: ev.seq, kind: ev.type, actor: 'agent', payload: ev.data ?? {}, at: new Date().toISOString() }
}

export function apply(ctx: Context, config: Config = {}): void {
  const { controlPlaneURL, runID, runToken } = config
  if (controlPlaneURL === undefined || runID === undefined || runToken === undefined) return

  const client = buildClient(controlPlaneURL, runID, runToken)
  ctx.provide('afControl', client)

  let buffer: SessionEvent[] = []
  const flushIntervalMs = config.flushIntervalMs ?? DEFAULT_FLUSH_INTERVAL_MS

  const flush = (): void => {
    if (buffer.length === 0) return

    const batch = buffer
    buffer = []
    void client.postEvents(batch.map(asMirrorEvent)).catch((error: unknown) => {
      ctx.logger.warn(`af-control: mirror flush failed: ${String(error)}`)
    })
  }

  const timer = setInterval(flush, flushIntervalMs)
  ctx.effect(() => () => { clearInterval(timer); flush() })

  ctx.effect(() => ctx.on('session/event', (_session, event) => {
    buffer.push(event)
  }))
}

declare module '@deepseek-ai/cordis' {
  interface Context {
    afControl: RunClient
  }
}
