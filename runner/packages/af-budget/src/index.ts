// af-budget: reads the token meter on `agent/pre-step` and cancels the agent
// on cost/minute/question breach (development-plan.md §5, M4).

export const name = 'af-budget'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M4): ctx.on('agent/pre-step', ...) check usd/minutes/questions;
  // agent.cancel({ kind: 'hook' }) on breach.
}
