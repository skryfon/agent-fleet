// af-ask-human: registers `ask_human` / `ask_orchestrator` tools and injects
// resumed answers via agent.inject() — an answer is data, never an
// instruction (development-plan.md §5, §6).

export const name = 'af-ask-human'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M3): ctx.tools.register(...) for ask_human/ask_orchestrator; on
  // resume, agent.inject() the durable answer as a tool result.
}
