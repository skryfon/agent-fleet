# 0016. The M4 egress proxy terminates TLS

Status: accepted
Decision: development-plan.md M4 (§4, §7, §8), layer 4 of ADR 0003

## Context

ADR 0003's fourth merge-prevention layer is "egress-proxy filtering of `PUT
/repos/*/pulls/*/merge` at the network layer." That filter needs to see the
HTTP method and path of every request a runner makes to `api.github.com` — a
plain CONNECT-tunneling proxy (tinyproxy, squid without SSL-bump) sees only
the destination host on an HTTPS connection, never the path, because the
TLS handshake happens end-to-end through it. A host-only allowlist can say
"this runner may talk to api.github.com" but cannot say "this runner may not
PUT to its merge endpoint."

Layer 4 is meaningless without path-level filtering, so the proxy must
terminate TLS: decrypt, inspect, filter, and re-encrypt on egress.

## Decision

The egress proxy (`deploy/egress-proxy/`) is mitmproxy, running in regular
(explicit HTTP_PROXY) mode with its own CA. Every runner container trusts
that CA (baked into `deploy/runner.Dockerfile` at build time via
`update-ca-certificates` + `NODE_EXTRA_CA_CERTS`), so `git`/`gh`/Node's
`fetch` all transparently route through it without per-tool configuration.
`addon.py` denies (1) any host outside development-plan.md §8's allowlist,
(2) `PUT /repos/*/pulls/*/merge` on api.github.com specifically, even though
that host is otherwise allowed.

The CA is generated once (`make egress-ca`) and persisted on a bind-mounted
volume (`deploy/egress-proxy/ca/`), never regenerated on restart or
redeploy — see that Makefile target's own comment for why a fresh CA would
break every already-built runner image's trust store.

## Consequences

- **The proxy sees plaintext.** Every credential a runner presents over
  HTTPS — the GitHub App installation token, the OmniRoute API key — passes
  through mitmproxy decrypted. This is the deliberate tradeoff this ADR
  records: `egress-proxy` becomes a component that must be trusted with
  those secrets in flight, same trust tier as the runner itself. It runs on
  no network but `[runners, egress]` (never `[core]`), holds no credentials
  of its own, and its only job is pass-through-or-403 — no logging of
  request bodies, no storage.
- **The CA private key is a real secret.** `deploy/egress-proxy/ca/` is
  gitignored (`.gitignore`'s M4 entry) with the same posture as `.env` — a
  leaked CA key lets anything that obtains it MITM every runner's traffic
  undetected. Back it up like any other production secret; losing it means
  re-running `make egress-ca` and rebuilding every runner image.
- **A CA rotation is a coordinated operation**, not a config change: it
  requires regenerating `ca/`, rebuilding `agentfleet-runner`, and
  redeploying `egress-proxy` together, or already-running/queued runner
  containers built against the old CA fail every outbound TLS connection.
- This layer denies by path pattern only (`PUT .../pulls/*/merge`) — it does
  not attempt to parse or understand GraphQL mutations against
  `api.github.com/graphql` that might achieve the same effect. `af-policy`
  and `internal/policy`'s hard-deny (layers 2-3) are what actually cover
  that gap; this layer's job is narrower and REST-shaped on purpose, per
  ADR 0003's "the fourth verified by manual test" framing — it is a backstop,
  not the primary guarantee (branch protection, layer 1, is).
