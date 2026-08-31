// af-worktree: confines all filesystem/subprocess IO to
// /workspace/<run-id> (development-plan.md §5, D2).

export const name = 'af-worktree'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M1): configure ctx.fs / ctx.subprocess roots to /workspace/<run-id>;
  // git worktree add + branch on boot.
}
