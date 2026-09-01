import { describe, expect, it } from 'vitest'
import { waitForAnswer, type RunClient } from './index.js'

// Pure-function tests, matching af-policy's violation()-style testing
// convention: no Cordis, no HTTP — a fake RunClient stands in.

function fakeClient(responses: Array<Awaited<ReturnType<RunClient['pollInboxOnce']>>>): RunClient {
  let i = 0

  return {
    dispatchTool: async () => ({ allow: true }),
    checkpoint: async () => {},
    pollInboxOnce: async () => responses[Math.min(i++, responses.length - 1)],
  }
}

describe('waitForAnswer', () => {
  it('returns the answer as soon as a matching message arrives', async () => {
    const client = fakeClient([undefined, { kind: 'answer', question_id: 'q1', answer: 'yes' }])

    const answer = await waitForAnswer(client, 'q1', 0, 10_000, new AbortController().signal)

    expect(answer).toBe('yes')
  })

  it('ignores an answer for a different question', async () => {
    const client = fakeClient([{ kind: 'answer', question_id: 'other', answer: 'no' }])

    const answer = await waitForAnswer(client, 'q1', 0, 1, new AbortController().signal)

    expect(answer).toBeUndefined()
  })

  it('returns undefined once the total wait budget elapses', async () => {
    const client = fakeClient([undefined])

    const answer = await waitForAnswer(client, 'q1', 0, 1, new AbortController().signal)

    expect(answer).toBeUndefined()
  })

  it('returns undefined immediately on an aborted signal', async () => {
    const client = fakeClient([{ kind: 'answer', question_id: 'q1', answer: 'yes' }])
    const controller = new AbortController()
    controller.abort()

    const answer = await waitForAnswer(client, 'q1', 0, 10_000, controller.signal)

    expect(answer).toBeUndefined()
  })
})
