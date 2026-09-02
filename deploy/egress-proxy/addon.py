"""
Egress allowlist + merge-endpoint filter — layer 4 of D3's four-layer
merge-prevention story (development-plan.md §4 M4, §8). Two independent
rules, both fail-closed (deny wins over allow):

1. Host allowlist (§8's literal list). Everything else gets a 403 before
   the request ever leaves this proxy.
2. `PUT /repos/{owner}/{repo}/pulls/{number}/merge` on api.github.com is
   denied even though api.github.com itself is allow-listed — "No merge
   tool exists" (D3) holds even if every upstream layer (af-policy,
   internal/policy's hard_deny) is somehow bypassed.

Both denials log one line mitmdump's own stdout captures (compose log),
matching internal/policy's own "Denials log as policy_violation" note in
development-plan.md §8 — this layer has no HTTP callback to the control
plane (the runner making the call has already been cut off by definition),
so a log line is the whole story for this layer specifically; layers 1-3
each already produce their own policy_violation event.
"""

import re

from mitmproxy import http

# development-plan.md §8's own literal egress allowlist.
HOST_ALLOWLIST = frozenset({
    "api.deepseek.com",
    "api.anthropic.com",
    "github.com",
    "api.github.com",
    "proxy.golang.org",
    "sum.golang.org",
    "registry.npmjs.org",
    "pypi.org",
})

MERGE_PATH = re.compile(r"^/repos/[^/]+/[^/]+/pulls/\d+/merge$")


def deny(flow: http.HTTPFlow, reason: str) -> None:
    flow.response = http.Response.make(403, reason.encode(), {"Content-Type": "text/plain"})
    print(f"egress-proxy: policy_violation denied host={flow.request.pretty_host} "
          f"method={flow.request.method} path={flow.request.path} reason={reason}")


def request(flow: http.HTTPFlow) -> None:
    host = flow.request.pretty_host

    if host not in HOST_ALLOWLIST:
        deny(flow, f"host {host} is not on the egress allowlist")
        return

    path = flow.request.path.split("?", 1)[0]
    if host == "api.github.com" and flow.request.method == "PUT" and MERGE_PATH.fullmatch(path):
        deny(flow, "PUT .../pulls/*/merge is never permitted through this proxy (D3: no merge tool exists)")
        return
