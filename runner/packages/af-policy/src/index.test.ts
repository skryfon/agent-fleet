import { describe, expect, it } from 'vitest'
import { violation } from './index.js'

const config = {
  deny: ['merge', 'gh_pr_merge'],
  denyBashPatterns: ['gh\\s+pr\\s+merge', '\\bgit\\s+push\\b.+\\b(main|master)\\b'],
}

describe('violation', () => {
  it('denies a tool named in the deny list', () => {
    expect(violation({ name: 'gh_pr_merge', arguments: {} }, config)).toMatch(/not permitted/)
  })

  it('denies a bash command matching a forbidden pattern', () => {
    expect(violation({ name: 'bash', arguments: { command: 'gh pr merge 42 --squash' } }, config))
      .toMatch(/forbidden pattern/)
  })

  it('allows and delegates an ordinary bash command', () => {
    expect(violation({ name: 'bash', arguments: { command: 'go test ./...' } }, config)).toBeUndefined()
  })

  it('allows and delegates a tool not on any list', () => {
    expect(violation({ name: 'create_branch', arguments: { name: 'x' } }, config)).toBeUndefined()
  })
})
