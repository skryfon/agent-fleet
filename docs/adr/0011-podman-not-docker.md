# 0011. Podman, not Docker, as the container runtime

Status: accepted
Decision: development-plan.md D11

## Context

Docker's daemon model means a single host-wide privileged `dockerd` that anything with
socket access can use to do essentially anything as root — the standard mitigation is
a `docker-socket-proxy` sidecar that filters the API surface. That's an extra service
to run and keep correctly configured. Podman is rootless and daemonless by default:
there is no host-wide privileged daemon to front with a proxy in the first place.

## Decision

Podman, not Docker, is the container runtime for the entire deployment. `supervisor`
is the **only** service with access to the (rootless, per-user) Podman socket
(`podman system service`), bind-mounted into it and nowhere else — retained from the
original locked decision rather than reverting to Docker + socket-proxy.

## Consequences

- Least privilege comes structurally from the rootless socket being per-user, not from
  an API filter someone has to maintain and audit — one less service, one less thing
  that can silently regress.
- `podman compose` is not guaranteed 100% API-compatible with `docker compose`;
  compose files in this repo (`deploy/*.compose.yaml`) are written and tested against
  `podman compose` specifically, not assumed portable to Docker without verification.
- Runner containers get the full hardening stack on top of this (§8): read-only
  rootfs, tmpfs `/tmp`, no capabilities, seccomp default, no host mounts — Podman's
  rootless model is the foundation these sit on, not a substitute for them.
- Anyone reaching for Docker tooling, docs, or muscle memory (`docker run`, socket
  paths, `docker-compose.yml` conventions) needs to translate — a real onboarding cost,
  paid once, versus the ongoing cost of maintaining a socket-proxy.
