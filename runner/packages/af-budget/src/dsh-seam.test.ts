// af-budget deliberately duck-types dsh-token-meter instead of importing it
// (see index.ts's head comment) so a rename or signature change there is
// invisible to `tsc -b` on af-budget's own project — the plugin just stops
// enforcing budgets, silently. `@deepseek-ai/dsh-token-meter` is a
// devDependency of THIS package for exactly this file: a type-only,
// compile-time-only check that our duck type still matches the real
// contract. It adds no runtime coupling — nothing here is imported by
// index.ts, and `it()` bodies never run TokenMeter code.
//
// Run as part of `pnpm run typecheck` (compiled) and `npx vitest run`
// (executed) — see the M4.5 upgrade drill (development-plan.md) for why
// this file exists: it is the regression detector for dsh's untyped seams
// that a dump-config diff and `tsc` alone would miss.
import type { TokenMeter } from '@deepseek-ai/dsh-token-meter'
import { describe, expect, it } from 'vitest'

// The exact shape index.ts casts `ctx.get('tokenMeter')` to.
interface TokenMeterService {
  measure(session: unknown): { totalTokens: number }
}

// Compile-time only: fails `tsc -b` if TokenMeter.measure's signature or
// TokenMeasurement.totalTokens moves. `session: unknown` accepting dsh's
// real `Session` is the direction af-budget actually calls it in.
type _AssertDuckTypeStillMatches = TokenMeter extends TokenMeterService ? true : never
const _assertion: _AssertDuckTypeStillMatches = true

describe('dsh-token-meter seam', () => {
  it('the duck-typed interface still assigns from the real TokenMeter (see the type-level assertion above)', () => {
    // The real check already ran at compile time. This just keeps the file
    // from being an empty describe block and fails loudly if it's ever
    // deleted without also deleting the type assertion.
    expect(_assertion).toBe(true)
  })
})
