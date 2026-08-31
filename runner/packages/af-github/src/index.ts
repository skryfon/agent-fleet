// af-github: branch, commit, push, and open-PR tools via `gh`. No merge tool
// exists in this package, deliberately (development-plan.md §5, D3).

export const name = 'af-github'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M1): ctx.tools.register(defineTool({ name: 'create_branch', ... }))
  // first (per development-plan.md week-one item 8), then commit/push/gh_pr_create.
}
