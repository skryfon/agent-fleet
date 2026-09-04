// af-context: `agent/pre-step` resolves hash-pinned spec_refs (D8) and
// rejects the step on hash mismatch (development-plan.md §5).
//
// AF_SPEC_REFS is the task's `spec_refs` column, JSON-encoded and passed
// straight through by internal/supervisor.RunLaunch and
// cmd/supervisor/daemon.go's spec() — see internal/domain/tasksmd.SpecRef
// for the {path, anchor, sha256} shape and internal/domain/tasksmd/schema
// for validation at ingest time. Verifying the hash here, not there, is the
// point of D8: the control plane never reads the spec's bytes, only pins a
// hash to it; the runner is what proves the worktree still matches.
//
// Anchor semantics (undefined by the plan; this is the one rule that
// matches what tasks.md spec_refs actually point at — a markdown section):
// omitted anchor hashes the whole file; a given anchor hashes from the
// heading whose slug matches it through the line before the next heading of
// equal-or-higher level (GitHub's own heading-to-slug rule: lowercase,
// spaces to hyphens, strip everything but [a-z0-9-]).
//
// ponytail: reads `afControl` via ctx.get at call time, not Cordis `inject`
// — af-control no-ops outside a real run, same reasoning as af-ask-human's
// and af-policy's own head comments.
//
// ponytail: latches on first mismatch (like af-budget's `breached`) rather
// than re-verifying every step — the worktree doesn't change out from under
// a run without af-github writing to it, and re-hashing every file on every
// step is needless IO for a check whose answer can't un-fail.

import { readFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import type { Context } from '@deepseek-ai/cordis'
import type { PreStepDecision } from '@deepseek-ai/dsh-agent'

export const name = 'af-context'

// Duplicated (not imported) from af-control's own module — same
// tsc -b --noEmit reasoning as af-ask-human's and af-policy's head comments.
interface RunClient {
  reportViolation(tool: string, reason: string): Promise<void>
}

export interface SpecRef {
  path: string
  anchor?: string
  sha256: string
}

export interface Config {
  /** JSON-encoded SpecRef[] — AF_SPEC_REFS, the task's spec_refs column. */
  specRefs?: string
  /** This run's worktree directory — AGENTFLEET_WORKTREE_DIR. */
  worktreeDir?: string
}

/** Pure so it table-tests without booting cordis or a real worktree. */
export function checkRef(ref: SpecRef, readFile: (path: string) => string): string | undefined {
  let content: string

  try {
    content = readFile(ref.path)
  } catch (error) {
    return `${ref.path}: could not read file: ${String(error)}`
  }

  const slice = ref.anchor === undefined ? content : sectionOf(content, ref.anchor)
  if (slice === undefined) {
    return `${ref.path}#${ref.anchor}: anchor not found`
  }

  const actual = createHash('sha256').update(slice).digest('hex')
  if (actual !== ref.sha256) {
    return `${ref.path}${ref.anchor === undefined ? '' : '#' + ref.anchor}: sha256 mismatch (expected ${ref.sha256}, got ${actual})`
  }

  return undefined
}

/** GitHub's own heading-to-slug rule: lowercase, spaces to hyphens, strip
 * everything but [a-z0-9-]. */
function slugify(heading: string): string {
  return heading.trim().toLowerCase().replace(/\s+/g, '-').replace(/[^a-z0-9-]/g, '')
}

/** Returns the section from the heading matching `anchor` through the line
 * before the next heading of equal-or-higher level, or undefined if no
 * heading slugs to `anchor`. */
function sectionOf(content: string, anchor: string): string | undefined {
  const lines = content.split('\n')
  const headingRe = /^(#{1,6})\s+(.*)$/

  let startIdx: number | undefined
  let startLevel = 0

  for (let i = 0; i < lines.length; i++) {
    const match = headingRe.exec(lines[i])
    if (match === null) continue
    if (slugify(match[2]) !== anchor) continue
    startIdx = i
    startLevel = match[1].length
    break
  }

  if (startIdx === undefined) return undefined

  let endIdx = lines.length
  for (let i = startIdx + 1; i < lines.length; i++) {
    const match = headingRe.exec(lines[i])
    if (match !== null && match[1].length <= startLevel) {
      endIdx = i
      break
    }
  }

  return lines.slice(startIdx, endIdx).join('\n')
}

export function apply(ctx: Context, config: Config = {}): void {
  if (config.specRefs === undefined || config.worktreeDir === undefined) return

  const { worktreeDir } = config

  let refs: SpecRef[]

  try {
    refs = JSON.parse(config.specRefs) as SpecRef[]
  } catch (error) {
    ctx.logger.warn(`af-context: AF_SPEC_REFS is not valid JSON, ignoring: ${String(error)}`)
    return
  }

  if (refs.length === 0) return

  let violation: string | undefined

  ctx.on('agent/pre-step', async (_input, next): Promise<PreStepDecision> => {
    if (violation !== undefined) return { kind: 'reject' }

    for (const ref of refs) {
      // checkRef's readFile is handed the ref's own relative path; this
      // wrapper resolves it against worktreeDir before ever touching disk.
      const reason = checkRef(ref, relPath => readFileSync(joinWorktree(worktreeDir, relPath), 'utf8'))

      if (reason !== undefined) {
        violation = reason
        break
      }
    }

    if (violation !== undefined) {
      ctx.logger.warn(`af-context: spec_ref mismatch — rejecting: ${violation}`)

      const client = ctx.get('afControl') as RunClient | undefined
      void client?.reportViolation('agent/pre-step', `spec_ref mismatch: ${violation}`).catch((error: unknown) => {
        ctx.logger.warn(`af-context: reporting violation failed: ${String(error)}`)
      })

      return { kind: 'reject' }
    }

    return next()
  })
}

function joinWorktree(worktreeDir: string, path: string): string {
  return `${worktreeDir.replace(/\/+$/, '')}/${path.replace(/^\/+/, '')}`
}
