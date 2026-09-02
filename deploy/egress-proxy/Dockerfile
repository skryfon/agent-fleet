# egress-proxy: mitmproxy on the [runners, egress] networks (compose.yaml),
# the only route a runner container has out (development-plan.md §8: "runners
# is internal: true. A runner's only routes out are the control-plane API
# and the egress proxy"). TLS-terminating so addon.py can actually inspect
# the request path — see docs/adr/0016-egress-proxy-terminates-tls.md for
# why that tradeoff (the CA sees every credential in flight) was accepted
# over a CONNECT-only allowlist that can't see paths at all.
FROM docker.io/mitmproxy/mitmproxy:11.1.3

COPY addon.py /addon.py

# confdir (mounted from ./ca — see compose.yaml) is where mitmproxy reads/
# writes its CA. `make egress-ca` seeds it once, out of band, so the CA
# baked into deploy/runner.Dockerfile's runtime image (a build-time COPY)
# stays byte-identical to the one this proxy actually signs with — a
# regenerated CA would otherwise silently break every already-built runner
# image's trust store.
ENTRYPOINT ["mitmdump", "--mode", "regular", "--listen-port", "8080", \
            "--set", "confdir=/home/mitmproxy/.mitmproxy", \
            "--set", "block_global=false", \
            "-s", "/addon.py"]
