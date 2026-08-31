// af-policy: `tools/pre-execute` waterfall denying merge, protected-branch
// push, and out-of-scope writes; emits `policy_violation` (development-plan.md
// §5). Second line of defense after the manifest's tool allow-list.
//
// TODO(M1): depend on @deepseek-ai/cordis; prove denial works from this
// plugin without patching dsh-base (development-plan.md M1 kill criterion).

export const name = 'af-policy'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M1): ctx.on('tools/pre-execute', ...) -> allow | deny | next()
}
