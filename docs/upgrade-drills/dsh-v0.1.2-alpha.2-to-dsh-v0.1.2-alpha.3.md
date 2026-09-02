# Upgrade drill: dsh-v0.1.2-alpha.2 → dsh-v0.1.2-alpha.3

M4.5 (development-plan.md). First run of `docs/dsh-upgrade-drill.md`'s runbook — that
file didn't exist yet; it was written from this run.

**Old pin:** `0a53fb55bea101816fa226bb964ae2bed71c343b` (dsh-v0.1.2-alpha.2)
**New pin:** `dd6322d604e00eec1ba5e0c8541159906a21094a` (dsh-v0.1.2-alpha.3)
**`SESSION_FORMAT_VERSION`:** `0` on both — unchanged by this hop.

## Time by category

Session-based estimate, not a stopwatch — this was one continuous sitting, not a clean
lab measurement. Recorded honestly as a lower bound: a from-scratch repeat, without the
detector infrastructure this run also built, would be faster on the mechanical steps and
about the same on the investigation.

| Category | Outcome | Time |
|---|---|---|
| 1. Toolchain | No drift — `packageManager`/`engines` identical on both pins | ~0 |
| 2. Typed drift | **Zero** compile errors across all six typed dsh imports (`Context`, `Agent`, `PreStepDecision`, `defineTool`/`PreToolDecision`/`ToolExecution`, `SessionEvent`/`SessionId`, `createUserMessage`, `brandString`) | ~5 min to run and confirm |
| 3. Build-script drift | `pnpm run build:lib` succeeds unchanged on the host in both a fresh (`pnpm run clean` first) and incremental tree | ~10 min |
| 4. Silent drift | `dump-config.sh --check`: **zero-diff** against the golden — every `af-*` row, every patched `dsh-bundle-headless` row id, unchanged byte-for-byte. `dsh-seam.test.ts`'s `tokenMeter` type-assignability check: unaffected. | ~5 min |
| Container build investigation | **~1.5–2 hours** — see below | |
| D1 session-resume test | Pass on both pins, ~15 min including mock-server setup | |

**Total attributable to this specific bump: under an hour.** The container-build time
was spent establishing that the failure is *not* attributable to the bump (see below) —
real time spent, but it's a fixed environment-diagnosis cost, not a per-bump recurring
one once `docs/dsh-upgrade-drill.md`'s control-step guidance exists.

## What tsc/vitest/dump-config caught vs. what only a live run caught

Nothing broke that only a live boot would have caught — `dump-config.sh --check` came
back with a literal zero-line diff, which is itself informative: at this hop, the
composition-level detector built in Step 1 wasn't exercised by any real drift. Its value
is proven by the earlier sanity check during Step 1's own construction (a deliberately
renamed `af-policy` row was caught immediately), not by this run.

The one thing that broke — the container build — was caught only by attempting
`make runner-image` for real, and even then required an explicit control test (build the
OLD pin with the same unmodified Dockerfile) to attribute correctly. **This is the
argument for keeping Step 4's control step mandatory**, not optional: without it, this
run would have filed a false "dsh-v0.1.2-alpha.3 breaks the container build" finding.

## Container build: pre-existing, not a regression

`make runner-image` (`podman build -f deploy/runner.Dockerfile`) failed deterministically
on this machine's podman setup (rootless Podman machine, applehv, arm64, 4 CPU/8GB):

```
ERROR  Error: [@deepseek-ai/dsh-root] Cannot find entry: ["lib/types/{index,invariant,startup}.js"]
    at resolveEntry (.../tsdown/dist/options-....mjs:83:34)
```

Investigation (in order):
1. Reproduced twice on the new pin — deterministic, not flaky.
2. Reproduced with `node:24-bookworm-slim` swapped in for the pinned `node:22.19` — not
   a Node-version issue.
3. **Reproduced identically on the OLD pin (`dsh-v0.1.2-alpha.2`) with the unmodified,
   already-shipped `deploy/runner.Dockerfile`** — the same Dockerfile `runner/README.md`
   documents as proven live for M1. This is the decisive test: the container build is
   broken independent of this bump, on this machine, right now.
4. A fully clean host build (`pnpm run clean && pnpm run build:lib`, no container)
   succeeds on both pins without this error — the break is specific to the Linux
   container filesystem/build environment, not the source.

**Conclusion:** this is a pre-existing environment issue (podman/tsdown/overlayfs
interaction on this machine), not a finding about dsh-v0.1.2-alpha.3. It blocks this
run's container-based acceptance checks (a live PR via `af-github`, the full
`docs/m4-merge-drill.md`, and an observed `af-budget` cap breach under a real container)
regardless of which dsh version is pinned. Filed separately as follow-up — not part of
this drill's scope to fix, and not evidence against the bump itself.

**Not independently reproduced elsewhere** — this machine only. Before trusting "podman
build is broken here" as the final word, the natural next check is whether it also fails
in CI (a fresh Linux runner, not a macOS Podman VM) or on a different engineer's machine;
that would separate "this Podman/tsdown combination" from "this one VM."

## Verified

- [x] `runner: typecheck && build && vitest run` — clean on the new pin
- [x] `go build/vet/test ./...` — clean and untouched (D13 holds — no Go file needed a
      change for a dsh bump, confirmed rather than assumed)
- [x] `dump-config.sh --check` — zero diff
- [x] A pre-bump session (captured against the dsh mock LLM server, no provider key
      needed) resumes cleanly via `af-resume` on the new pin
- [ ] Live container run opens a real PR — **blocked**, pre-existing environment issue,
      not evidence about this dsh version (see above)
- [ ] `docs/m4-merge-drill.md` end-to-end — **blocked**, same reason
- [ ] `af-budget` token-cap breach kills a real run — **blocked**, same reason (needs a
      container + a live control plane; not exercised as a pure host process)

## Files touched by the bump itself

None, beyond `deepseek-harness`'s own pin (a submodule commit bump) and
`.gitmodules`/submodule remote fixed to HTTPS as a prerequisite (unrelated to the dsh
version; needed because this machine's SSH key isn't authorized for
`git@github.com:deepseek-ai/deepseek-harness.git`).

## Recommendation for development-plan.md §10's upgrade-cost metric

Log this as **"under 1 engineer-hour for the bump itself; ~2 hours to diagnose an
unrelated, pre-existing container-build environment issue that this drill happened to
surface."** Don't count the second number against dsh's compatibility record — it would
have blocked M1's original container proof too, on this machine, at either version.
