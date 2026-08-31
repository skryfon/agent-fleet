// af-control: mirrors dsh `session/event` to the control plane and long-polls
// the run inbox for answers/cancellations (development-plan.md §5, §4).
//
// TODO(M1): depend on @deepseek-ai/cordis (from the vendored deepseek-harness
// checkout) and type `ctx` as Context. Left untyped until that dependency is
// wired so this scaffold has no external module resolution to satisfy yet.

export const name = 'af-control'

export function apply(_ctx: unknown, _config: unknown) {
  // TODO(M1): inject ['agents']; listen 'session/event', batch, POST to
  // /v1/runs/{id}/events; long-poll GET /v1/runs/{id}/inbox.
}
