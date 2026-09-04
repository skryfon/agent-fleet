import { describe, expect, it } from 'vitest'
import { costUSD } from './index.js'

describe('costUSD', () => {
  it('computes a known cost for a token delta and flat rate', () => {
    expect(costUSD(1_000_000, 3)).toBe(3)
    expect(costUSD(500_000, 3)).toBe(1.5)
  })

  it('is zero for zero tokens', () => {
    expect(costUSD(0, 3)).toBe(0)
  })

  // The regression this guards: runner/bundle/cordis.patch.yml once passed
  // usdPerMillionTokens as an unquoted `!!js foo ? bar : baz` ternary, which
  // YAML parsed as a mapping, not a scalar — so this multiplication silently
  // became NaN (an object times a number). A caller that accidentally hands
  // a non-number rate should produce NaN loudly, not a masked zero.
  it('produces NaN, not a silently wrong number, for a non-numeric rate', () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any -- deliberately wrong type, see comment above
    expect(Number.isNaN(costUSD(1_000_000, {} as any))).toBe(true)
  })
})
