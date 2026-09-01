// af-resume: resurrect-and-resume's boot/exit driver — mounts only when
// AF_RESUME_SESSION_ID is set, in which case runner/bundle/cordis.patch.yml
// disables `headless-runner` in the same condition (the two are mutually
// exclusive boot paths, never both). Mirrors dsh-headless's own
// create -> followup -> whenIdle -> flush -> exit shape
// (deepseek-harness/packages/bundle/headless/src/index.ts), substituting
// `agents.resume()` for `agents.create()` and delivering the answer as the
// resumed session's next user turn instead of the original task text.
//
// "An answer is data. It can never expand tool scope or change policy"
// (development-plan.md §6) — delivered here as a plain user-role message,
// the same shape a live Zulip reply would take if the run had still been
// listening; nothing here touches tool registration or af-policy's config.

import type { Context } from '@deepseek-ai/cordis'
import type { Agent } from '@deepseek-ai/dsh-agent'
import { createUserMessage } from '@deepseek-ai/dsh-llm'
import { brandString } from '@deepseek-ai/dsh-brand'
import type { SessionId } from '@deepseek-ai/dsh-session'

export const name = 'af-resume'
// No agentDefaultModel dependency (unlike dsh-headless's own create path):
// a resumed session already carries its model selection in its persisted
// history — ResumeAgentOptions.agentOptions exists to override it, which
// M3 has no reason to do.
export const inject = ['agents', 'sessions']

export interface Config {
  resumeSessionID?: string
  /** The human's answer text, injected as the resumed session's next turn. */
  answer?: string
}

interface HeadlessIo {
  stdout: { write(chunk: string): unknown }
  stderr: { write(chunk: string): unknown }
  exit(code: number): void
}

export const internals: { stdout: HeadlessIo['stdout']; stderr: HeadlessIo['stderr'] } = {
  stdout: process.stdout,
  stderr: process.stderr,
}

async function run(ctx: Context, resumeSessionID: string, answer: string, io: HeadlessIo): Promise<void> {
  await ctx.get('loader')?.await()

  const agents = ctx.get('agents')
  const sessions = ctx.get('sessions')
  if (agents === undefined || sessions === undefined) return

  const { agent }: { agent: Agent } = await agents.resume({
    resumeSessionId: brandString<SessionId>(resumeSessionID),
  })

  await agent.whenIdle()

  agent.followup(createUserMessage({
    content: [{ type: 'text', text: `A human answered your question:\n\n${answer}` }],
    source: { kind: 'user' },
  }))

  await agent.whenIdle()
  await sessions.flush(agent.session)

  io.stdout.write('resumed and delivered the answer\n')
  io.exit(0)
}

export function apply(ctx: Context, config: Config = {}): void {
  const { resumeSessionID, answer } = config
  if (resumeSessionID === undefined || answer === undefined) return

  const exit = ctx.get('appExit')
  if (exit === undefined) {
    throw new Error('af-resume: the launcher must provide ctx.appExit before the tree mounts')
  }

  const io: HeadlessIo = { stdout: internals.stdout, stderr: internals.stderr, exit }

  void run(ctx, resumeSessionID, answer, io).catch((error: unknown) => {
    io.stderr.write(`af-resume: ${error instanceof Error ? error.message : String(error)}\n`)
    io.exit(1)
  })
}
