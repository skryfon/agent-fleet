---
name: dsh-plugin-dev
description: Use when creating or editing a Cordis/dsh plugin in this repo — an af-* runner package, a dsh tool, a hook/policy listener, or any packages/<group>/<pkg> module. Covers plugin shape, package layout, event dispatch, effects/disposal, and tool-authoring contracts.
---

# dsh Plugin Development

`dsh` (vendored at `deepseek-harness/`) is an all-plugin Cordis harness. AgentFleet's
`runner/packages/af-*` packages ARE Cordis plugins that get bundled into the
`agentfleet-runner` profile (`dsh-bundle-agentfleet`) — see project CLAUDE.md D14.
This skill is the fast path for writing one correctly. It summarizes; the full
authority is `deepseek-harness/docs/` — read the linked file when a rule here isn't
enough.

## Cordis in five ideas

1. **A plugin is an object implementing `Service`** — usually a plain function with
   `name` / `inject` / `apply(ctx, config)`, or a `Service` subclass.
2. **`Context` is a repository of services** — a service claims `ctx.<key>`; other
   plugins reach it by key, never by importing the concrete class.
3. **`inject` declares dependency** — a plugin waits until every named service exists;
   load order comes from declared requirements, not manual sequencing.
4. **Typed events, via declaration merging** — dispatched `emit` (fire, unawaited,
   no return), `waterfall` (around-middleware, must call `next()` to delegate),
   `parallel` (awaited, fan-out), `serial` (awaited, ordered, has return), or `bail`
   (ordered, stops at first non-undefined return).
5. **Registrations are reversible effects** — every listener/tool/section goes through
   `ctx.effect()` or `ctx.on()` (which returns a disposer) so unmount/hot-reload
   unwinds cleanly.

Full primer: `deepseek-harness/docs/cordis-primer.md`.

## Minimal plugin shape

```ts
import type { Context } from '@deepseek-ai/cordis'

export const name = 'af-example'
export const inject = ['tools']          // wait for services this plugin needs

export interface Config {
  enabled: boolean
}

export function apply(ctx: Context, config: Config) {
  ctx.on('tools/pre-execute', async (exec, next) => {
    if (!config.enabled) return next()
    // ... policy decision
    return next()
  })
}
```

- `inject` names are Cordis service keys (`'tools'`, `'agents'`, `'sessions'`, …), not
  npm package names.
- A waterfall listener that doesn't own the decision **must** call and return
  `next()` — forgetting it silently short-circuits the chain (this repo's #1 bug
  source in hook plugins).
- Never hardcode a deployment-varying value; put it on `Config` and read it from
  `cordis.yml`/the profile patch. A `DEFAULT_*` constant is not configurability.

## Which extension point do I want?

| I want to... | Use |
|---|---|
| Register a model-callable tool | `ctx.tools.register(defineTool({...}))` |
| Allow/deny/ask before a tool runs | `tools/pre-execute` (waterfall, return a `PreToolDecision`) |
| A final, monotonic denial nothing can undo | `ctx.tools.guard()` |
| Wrap dispatch (timeout/retry/metrics) | `tools/execute` |
| Replace result content / block / attach context | `tools/post-execute` |
| Observe the immutable final outcome | `tools/result` |
| React to the assistant token/event stream | `ctx.on('session/event', ...)` |
| Feed the model async context (not a wake-up) | `exec.agent.inject({ content, source: {...} })` |
| Push a new user/system turn | `agent.followup()` / `agent.steer()` |
| Add a system-prompt section | `ctx.systemPrompt.section(...)` |
| Long-running background work | `ctx.jobs.start({ kind, label, owner: exec.agent, run })` |

Full feature→mechanism map (hooks, workflow, subagents, MCP, compaction, plan mode,
scheduling, etc.): `deepseek-harness/docs/cookbook/extension-cookbook.md`.

## Writing a tool

```ts
import { defineTool } from '@deepseek-ai/dsh-tools'

ctx.tools.register(defineTool({
  name: 'af_example',
  description: 'What the model sees.',
  parameters: { path: { type: 'string', required: true } },
  output: {
    schema: { type: 'string' },
    render: (_args, value) => [{ type: 'text', text: value }],
  },
  async execute(args, exec) {
    // args is typed + already validated against the schema.
    // Honor exec.signal; return the canonical value, don't throw for
    // ordinary domain outcomes (throw only for infrastructure failure).
  },
}))
```

Rules that bite: args are validated for you (don't re-validate what the schema
already covers, do check things it can't — non-empty strings, cross-field rules);
`execute` returns the canonical value only, never content blocks; a UI card is a
separate, pure `presentCall`/`presentResult` projection (no I/O, no session reads —
runs on replay too). Full contract, background jobs, PTC mode, UI card kinds:
`deepseek-harness/docs/cookbook/adding-a-tool.md`.

## New package checklist (af-* or packages/<group>/<pkg>)

1. `package.json`: `private: true`, `type: module`, `main`/`types`/`exports` pointing
   at `lib/`, `@deepseek-ai/cordis` in both `peerDependencies` and `devDependencies`
   (same range as every other dsh peer you use).
2. `tsconfig.json` extends the workspace base, references the vendored `cordis`
   (+ `schemastery` if you declare `Config`) and any dsh package you depend on.
3. `src/index.ts` exports `name`, `inject`, `apply(ctx, config)` (+ `Config` type).
4. Register the package in the relevant `tsconfig.*.json` `references` array.
5. README with a package-specific API section — required `Model Experience` and
   `Known Limitations` sections only apply inside `deepseek-harness/` itself, not
   `runner/packages/af-*`, but document config, events, and extension points either
   way.
6. Verify: `pnpm install && pnpm run typecheck && pnpm run lint && pnpm run build`
   (inside `deepseek-harness/`, add `pnpm run constraints && pnpm run doc-sync &&
   pnpm run hygiene`).

Full checklist with naming-role table (Controller/Store/Registry/Runtime/Policy/…):
`deepseek-harness/docs/cookbook/adding-a-package.md`.

## AgentFleet-specific constraints (don't relitigate)

- Everything AgentFleet-specific lives in `dsh-bundle-agentfleet` (D14); patching a
  `dsh-base` config row by id needs architect sign-off.
- `af-*` packages are ordinary Cordis plugins per `deepseek-harness/docs/cookbook/
  adding-a-package.md` and `adding-a-tool.md` — this repo has no framework of its own.
- The `runners` network is `internal: true`: no plugin here can reach Postgres,
  Zulip, or the Podman socket directly — that's `af-control`'s job via the control
  plane HTTP API (development-plan.md §4).
- An in-process dsh subagent (nested `ctx.plugin`) is for short read-only helpers
  only. Anything needing a Run row, a budget, a depth limit, or its own PR must be
  spawned via `af-subagent`, never run in-process — the control plane can't see an
  in-process child.
- Community/unvetted Cordis plugins never run in a runner holding GitHub
  credentials without a named person's recorded source review.

## Quick sanity check before you call it done

- Does every `ctx.on`/`register()` call have a disposer path (via `ctx.effect()` or
  the registry's own effect)?
- Does every waterfall listener call `next()` unless it deliberately owns the
  decision?
- Is anything deployment-varying on `Config` instead of a hardcoded constant?
- Would `dsh --profile agentfleet-runner --dump-config` show this plugin's row and
  no `PENDING` fiber?
