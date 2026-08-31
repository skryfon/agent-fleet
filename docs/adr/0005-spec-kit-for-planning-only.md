# 0005. Spec Kit is used for planning only, never implementation

Status: accepted
Decision: development-plan.md D5

## Context

Spec Kit-style planning tools are good at producing structured specs and task
breakdowns from human intent, but running them as the actual execution engine would
mean a second agent framework alongside dsh — doubling the surface this project has to
trust and operate.

## Decision

Spec Kit produces planning artifacts only (specs, `tasks.md`). It never executes code,
opens PRs, or touches a repository beyond the planning artifacts themselves. All
implementation runs through the dsh runner (D12).

## Consequences

- The handoff between planning and execution is a single, explicit contract:
  `tasks.md`, produced by Spec Kit and committed via PR, schema-validated before
  ingestion (development-plan.md §1 "Handoff contract"). Nothing else crosses that
  boundary.
- Spec Kit output is reviewed and merged like any other PR — it does not get a
  privileged bypass of normal review just because it's machine-generated.
- Keeps D12/D13/D14's "one worker harness, isolated from the control plane" story
  intact: Spec Kit never becomes a second thing `af-*` plugins or the control plane
  need to integrate with at runtime.
