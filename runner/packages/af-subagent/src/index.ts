// af-subagent: `spawn_worker` over ctx.subagents, routed through the control
// plane for depth/fan-out limits (development-plan.md §5, M5). In-process
// children are for short read-only helpers only — anything needing a Run
// row, a budget, or its own PR must be spawned.

export const name = 'af-subagent'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M5): ctx.tools.register(spawn_worker) -> ctx.subagents provider
  // (subagent-dsh-sdk for out-of-process children); POST run creation to
  // the control plane first so depth/fan-out limits are enforced there.
}
