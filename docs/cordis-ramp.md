# Cordis ramp (M0)

development-plan.md §7 M0 is done, in part, when **both engineers can explain the dsh
boot tree from `--dump-config`**. "Two, not one — this knowledge must not be
single-homed." This doc is the checklist and the sign-off artifact for that.

For day-to-day plugin *writing* once M1 starts, use the `.claude/skills/dsh-plugin-dev`
skill instead of re-deriving this from `deepseek-harness/docs/` each time — it
summarizes the same seams this ramp covers.

## Reading order

All paths relative to `deepseek-harness/docs/`:

1. `architecture.md`
2. `cordis-primer.md`
3. `cordis-tutorial/01-first-plugin.md` through `07-into-the-harness.md`
4. `capability-seams.md`
5. `config-catalog.md`
6. `cookbook/extension-cookbook.md`
7. `cookbook/adding-a-package.md`
8. `cookbook/adding-a-tool.md`

## The exercise

From the vendored checkout:

```sh
cd deepseek-harness
dsh --profile dsh-headless --dump-config
```

Fill in the boot tree below from the actual output — which rows come from
`dsh-base`, which from `dsh-headless`, and where `dsh-bundle-agentfleet`'s inserts
(`runner/bundle/cordis.patch.yml`) will land once M1 wires `@deepseek-ai/cordis`. This
filled-in section **is** the M0 sign-off artifact — it's also the first thing M1 needs
to know before writing `af-policy`/`af-worktree`/`af-github`.

### Boot tree

<!--
Fill in after running --dump-config. Structure suggestion:

- dsh-base
  - <service/fiber id> — <what it provides>
  - ...
- dsh-headless (layers over dsh-base)
  - <service/fiber id> — <what it provides>
  - ...
- dsh-bundle-agentfleet (where our af-* inserts land, per cordis.patch.yml)
  - af-control    — after <anchor id>
  - af-context    — after <anchor id>
  - af-worktree   — after <anchor id>
  - af-github     — after <anchor id>
  - af-policy     — after <anchor id>
  - af-ask-human  — disabled until M3
  - af-budget     — disabled until M4
  - af-subagent   — disabled until M5
  - af-webhook    — disabled, optional M6+

Note any fiber that shows PENDING and why.
-->

TODO — fill in from `--dump-config` output.

## Sign-off

Per §7: two engineers, not one.

| Engineer | Date | Notes |
|---|---|---|
| | | |
| | | |
