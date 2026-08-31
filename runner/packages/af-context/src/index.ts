// af-context: `agent/pre-step` resolves hash-pinned spec_refs (D8) and
// rejects the step on hash mismatch (development-plan.md §5).

export const name = 'af-context'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M1/M2): ctx.on('agent/pre-step', ...) resolve spec_refs by
  // {path, anchor, sha256}; reject on mismatch.
}
