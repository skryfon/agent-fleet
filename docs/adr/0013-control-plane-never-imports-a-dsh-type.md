# 0013. The control plane never imports a dsh type

Status: accepted
Decision: development-plan.md D13

## Context

If the Go control plane imported dsh/Cordis types directly, every dsh upgrade would
risk a control-plane compile break, and the two systems' release cycles would be
coupled — a pre-release, frequently-changing dependency (see
`0012-dsh-is-the-worker-harness.md`) would leak its churn straight into the
deterministic core of the system.

## Decision

All contact between the control plane and the runner is the HTTP API described in
development-plan.md §4. The control plane, written in Go, never imports a dsh type or
links against the TypeScript/Cordis runtime in any form.

## Consequences

- The control plane can be developed, tested, and reasoned about entirely independent
  of dsh's release cadence — including with the mock-LLM runner (M2) for deterministic
  integration tests at zero token cost, without a real dsh process involved at all.
- A dsh upgrade (M4.5 drill) never requires touching `cmd/control-plane` or
  `internal/*` Go code for compilation reasons — only the HTTP contract matters, and
  that contract is versioned and tested on its own terms.
- Language boundary (Go vs. TypeScript) reinforces the discipline structurally: there
  is no accidental shared-type shortcut available even if someone wanted one.
- Combined with D14, this is what keeps D12's dsh dependency "swappable" in the
  literal sense the plan claims — if dsh had to be replaced (the M1 kill criterion), no
  control-plane code would need to change, only the runner side of the HTTP contract.
