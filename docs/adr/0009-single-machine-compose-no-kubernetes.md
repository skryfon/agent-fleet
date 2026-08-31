# 0009. Single self-hosted machine, Podman Compose, no Kubernetes

Status: accepted
Decision: development-plan.md D9

## Context

The team is 5–6 people running at most 3–4 concurrent agent runs (§8 — the ceiling is
human review capacity, not compute). Kubernetes solves problems of scale and
multi-node scheduling this project doesn't have, at the cost of an operations burden
(control plane, networking overlay, RBAC, upgrade cadence) this project can't spare
~1.5 FTE for.

## Decision

One machine (8 vCPU / 32 GB / 500 GB SSD, §8), Podman Compose, no Kubernetes, no
Temporal (D10). The compose topology in §8 — `edge`/`core`/`runners`/`egress` networks,
`runners` marked `internal: true` — is the whole deployment model.

## Consequences

- No horizontal scaling story beyond "bigger machine" — acceptable, since the review
  bottleneck is human, not compute, per §8.
- Failure modes are single-machine failure modes: a host outage takes down everything.
  Mitigated by the backup cadence in §8 (hourly/daily `pg_dump`, Zulip
  `manage.py backup`, tested restore quarterly), not by redundancy.
- Network isolation (the actual security boundary a Kubernetes NetworkPolicy would
  otherwise provide) is done with plain Compose networks: `runners` has no route to
  Postgres, Zulip, or the Podman socket, and reaches only the control-plane API and
  the egress proxy for external hosts.
- Revisit only if concurrency needs to exceed the review-capacity ceiling — not before.
