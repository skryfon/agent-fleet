import { createHash } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import { checkRef } from './index.js'

function sha256(s: string): string {
  return createHash('sha256').update(s).digest('hex')
}

const wholeFile = 'line one\nline two\n'
const doc = [
  '# Title',
  '',
  '## Section A',
  'content a',
  '',
  '## Section B',
  'content b',
  '### Nested',
  'nested content',
  '',
  '## Section C',
  'content c',
].join('\n')

function readerFor(files: Record<string, string>): (path: string) => string {
  return (path: string) => {
    const content = files[path]
    if (content === undefined) throw new Error(`ENOENT: ${path}`)
    return content
  }
}

describe('checkRef', () => {
  it('matches a whole-file hash', () => {
    const read = readerFor({ 'a.md': wholeFile })
    expect(checkRef({ path: 'a.md', sha256: sha256(wholeFile) }, read)).toBeUndefined()
  })

  it('rejects a whole-file hash mismatch', () => {
    const read = readerFor({ 'a.md': wholeFile })
    expect(checkRef({ path: 'a.md', sha256: sha256('tampered') }, read)).toMatch(/sha256 mismatch/)
  })

  it('matches an anchored section, stopping before the next same-or-higher heading', () => {
    const read = readerFor({ 'doc.md': doc })
    const section = ['## Section B', 'content b', '### Nested', 'nested content', ''].join('\n')
    expect(checkRef({ path: 'doc.md', anchor: 'section-b', sha256: sha256(section) }, read)).toBeUndefined()
  })

  it('rejects an anchored section hash mismatch', () => {
    const read = readerFor({ 'doc.md': doc })
    expect(checkRef({ path: 'doc.md', anchor: 'section-b', sha256: sha256('tampered') }, read)).toMatch(/sha256 mismatch/)
  })

  it('rejects an anchor that does not match any heading', () => {
    const read = readerFor({ 'doc.md': doc })
    expect(checkRef({ path: 'doc.md', anchor: 'nope', sha256: sha256('x') }, read)).toMatch(/anchor not found/)
  })

  it('rejects a missing file', () => {
    const read = readerFor({})
    expect(checkRef({ path: 'missing.md', sha256: sha256('x') }, read)).toMatch(/could not read file/)
  })
})
