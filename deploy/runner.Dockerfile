# agentfleet-runner: one container per Run (D2, development-plan.md §5).
# Builds the vendored dsh checkout + the af-* plugins from a clean checkout —
# deepseek-harness/vendor and packages/*/lib are gitignored there (native
# deps, so a host-built copy cannot cross architectures into the container).
#
# Build context is the repo root:
#   podman build -f deploy/runner.Dockerfile -t agentfleet-runner .
# Run (rootless, D11):
#   podman run --rm \
#     -e OMNI_ROUTE_API_KEY -e GH_TOKEN \
#     -e REPO_URL=https://github.com/<owner>/<repo>.git \
#     -e RUN_ID=<uuid> -e TASK="..." \
#     -v agentfleet-run-<id>:/workspace \
#     agentfleet-runner

# Preserve the real repo's sibling layout (agent_fleet/runner next to
# agent_fleet/deepseek-harness) at /opt in BOTH stages, so runner/packages/
# af-*'s relative `link:../../../deepseek-harness/...` devDependencies
# resolve identically at build time and at container runtime — no path
# rewriting needed.
FROM node:22.19-bookworm-slim AS build

RUN corepack enable
WORKDIR /opt

# deepseek-harness/tsconfig.host.json project-references the FULL workspace
# (including the optional @openai/codex / @anthropic-ai/claude-agent-sdk
# subagent-provider packages agentfleet-runner never uses), so `pnpm install
# --filter '!...'` to skip their 100MB+ platform binaries breaks `tsc -b`'s
# project graph — not an option without patching the vendored checkout
# (D14). Widen pnpm's retry budget instead: these are large tarballs over a
# constrained podman-VM network path, not a broken registry.
ENV PNPM_CONFIG_FETCH_RETRIES=10 \
    PNPM_CONFIG_FETCH_RETRY_MINTIMEOUT=20000 \
    PNPM_CONFIG_FETCH_RETRY_MAXTIMEOUT=180000 \
    PNPM_CONFIG_FETCH_TIMEOUT=180000 \
    PNPM_CONFIG_NETWORK_CONCURRENCY=4

COPY deepseek-harness ./deepseek-harness
# `build:lib:host` alone left some packages (e.g. dsh-typert-registry)
# without their lib/index.js — `apps/cli/README.md`'s documented production
# build is plain `pnpm run build`, which runs `build:lib` (host AND client
# faces) then `build:web`. We need `build:lib` for a correct lib/ tree but
# not `build:web` (the browser frontend; agentfleet-runner is headless-only).
RUN cd deepseek-harness && pnpm install --frozen-lockfile && pnpm run build:lib

COPY runner ./runner
RUN cd runner && pnpm install --frozen-lockfile && pnpm run build

# --- runtime image -----------------------------------------------------

FROM node:22.19-bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git curl ca-certificates \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && corepack enable \
    && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --shell /bin/bash agentfleet

# M4 egress proxy (development-plan.md §8, §4 layer 4): mitmproxy terminates
# TLS to filter `PUT /repos/*/pulls/*/merge`
# (deploy/egress-proxy/addon.py), so every outbound HTTPS call from inside
# this container must trust its CA — baked in at build time (a regenerated
# CA would otherwise break every already-built runner image; see
# docs/adr/0016). NODE_EXTRA_CA_CERTS covers Node's own fetch/https client
# (dsh's LLM/session calls, af-control's fetch calls); update-ca-certificates
# covers `git`/`gh`/curl.
COPY deploy/egress-proxy/ca/mitmproxy-ca-cert.pem /usr/local/share/ca-certificates/agentfleet-egress-proxy.crt
RUN update-ca-certificates
ENV NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/agentfleet-egress-proxy.crt

COPY --from=build /opt/deepseek-harness /opt/deepseek-harness
COPY --from=build /opt/runner /opt/runner
COPY deploy/runner-entrypoint.sh /opt/runner-entrypoint.sh
RUN chmod +x /opt/runner-entrypoint.sh

# Bake the agentfleet-runner profile at build time (network + failure surface
# belong at build, not at every container start): dsh-base + dsh-headless +
# our bundle, plus the OmniRoute settings.yaml (development-plan.md §5 — no
# af-llm-* plugin needed, see runner/README.md).
ENV DSH_HOME=/home/agentfleet/.dsh
RUN node /opt/deepseek-harness/apps/cli/lib/bin.js plugin --profile agentfleet-runner add \
      "link:/opt/deepseek-harness/packages/bundle/headless" \
      "link:/opt/runner/bundle" \
      "link:/opt/runner/packages/af-policy" \
      "link:/opt/runner/packages/af-github" \
      "link:/opt/runner/packages/af-worktree" \
      "link:/opt/runner/packages/af-control" \
      "link:/opt/runner/packages/af-ask-human" \
      "link:/opt/runner/packages/af-resume" \
      "link:/opt/runner/packages/af-budget" \
    && node -e "const fs=require('node:fs'); const p='$DSH_HOME/profiles/agentfleet-runner/package.json'; const m=JSON.parse(fs.readFileSync(p)); m.dsh.profile.patchReload='startup'; fs.writeFileSync(p, JSON.stringify(m,null,2)+'\n')"
COPY deploy/runner-settings.yaml $DSH_HOME/settings.yaml

RUN mkdir -p /workspace && chown -R agentfleet:agentfleet /home/agentfleet /workspace

USER agentfleet
WORKDIR /home/agentfleet
ENV AGENTFLEET_WORKSPACE_ROOT=/workspace

ENTRYPOINT ["/opt/runner-entrypoint.sh"]
