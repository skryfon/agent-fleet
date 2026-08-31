// af-webhook (optional, M6+): auto-starts a read-only review session on
// GitHub `ready_for_review`, modeled on dsh's own github-review example
// (development-plan.md §5, §0 merge notes).

export const name = 'af-webhook'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M6, optional): ctx.webhookRuntime.register({ id, kind: 'github', run })
  // mirroring deepseek-harness/apps/cli/config/examples/github-review/.
}
