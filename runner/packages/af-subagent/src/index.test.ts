import { describe, expect, it } from 'vitest'
import { spawnRoleViolation } from './index.js'

describe('spawnRoleViolation', () => {
  it('allows when spawnedRoles is undefined (pre-M6 / unrestricted)', () => {
    expect(spawnRoleViolation('reviewer', undefined)).toBeUndefined()
  })

  it('allows when spawnedRoles is empty', () => {
    expect(spawnRoleViolation('reviewer', [])).toBeUndefined()
  })

  it('allows when no role override was requested', () => {
    expect(spawnRoleViolation(undefined, ['reviewer'])).toBeUndefined()
  })

  it('allows a role present in spawnedRoles', () => {
    expect(spawnRoleViolation('reviewer', ['reviewer', 'implementer'])).toBeUndefined()
  })

  it('denies a role absent from spawnedRoles', () => {
    expect(spawnRoleViolation('orchestrator', ['reviewer'])).toMatch(/not in this role's manifest subagents\.spawned list/)
  })
})
