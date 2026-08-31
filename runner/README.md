# runner/

The `agentfleet-runner` dsh profile: `dsh-base` + `dsh-headless` +
`dsh-bundle-agentfleet` (development-plan.md §5). This directory is a pnpm
workspace scaffolded for M0; the plugins are unwired stubs.

## Layout

- `packages/af-*` — one Cordis plugin per use case (see each `src/index.ts`
  for its seam and milestone).
- `bundle/` — `dsh-bundle-agentfleet`, the patch that stacks the `af-*`
  plugins into a profile.

## What's not done yet (M1)

- None of these packages depend on `@deepseek-ai/cordis` yet. Wiring that
  dependency against the vendored `../deepseek-harness/` checkout, and
  standing up an actual `agentfleet-runner` profile that layers
  `dsh-bundle-agentfleet` over `dsh-base`/`dsh-headless`, is the first task
  of M1 (development-plan.md week-one items 6–9).
- Until then, `pnpm install` here only wires the local workspace — it does
  not yet install or link against dsh itself.
