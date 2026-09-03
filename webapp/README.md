# webapp/

M7 (development-plan.md §7): approval queue, live run view over SSE, and
cost/drift/question-rate dashboards. `dsh web` covers live run view,
transcripts, and diffs for free — this app is deliberately scoped to what
it doesn't cover, and links out to the PR itself rather than rendering a
diff.

Plain Vite + React + TypeScript — no router, no state library, no chart
library, no component library. Four views behind one tab switch.

## Running

```
npm install
npm run dev      # proxies /v1 and /healthz to localhost:8080 (vite.config.ts)
```

Paste the control plane's `ADMIN_TOKEN` (same one `deploy/compose.yaml`'s
`control-plane` service reads) when prompted. It's kept in the browser's
`localStorage`, never in the URL — `GET /v1/events` is read via `fetch` +
a streaming reader, not `EventSource`, specifically so the token can travel
as an `Authorization` header instead of a query parameter.

There is no separate identity auth in this milestone (GitHub/Zulip-backed
login is a named follow-up, not built here) — every approval records as
actor `api:approve`, same as a direct API call.

## Building

```
npm run build     # tsc && vite build -> dist/
```

`make webapp` (repo root) does this. `deploy/compose.yaml`'s `caddy`
service mounts `dist/` read-only and serves it alongside `/v1/*` and
`/healthz` proxied through to `control-plane` — the webapp is not its own
compose service.

## Tests

```
npx vitest run
```

The only non-trivial logic is `src/api.ts`'s `parseSSE` (splitting a
growing text buffer into complete SSE frames across chunk boundaries) —
`src/api.test.ts` covers it. The views are rendering, not logic.
