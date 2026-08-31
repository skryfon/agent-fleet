# Zulip setup (Zulip Cloud)

AgentFleet uses **Zulip Cloud** (`https://<org>.zulipchat.com`), not a
self-hosted instance — see `docs/adr/0004-zulip-as-primary-human-channel.md`
for why this superseded the original self-hosted decision. There is no
compose file here: Zulip's infrastructure, uptime, and backups are Zulip's
problem, not ours.

## 1. Org and bot

1. Use (or create) a Zulip Cloud org on the free plan.
2. Personal settings → Bots → Add a new bot → **Generic bot** (not an
   outgoing webhook bot — the bridge, M3's `af-ask-human`/`af-control`,
   receives messages via the real-time events API, not an inbound webhook,
   so it needs no public HTTPS endpoint).
3. Copy the bot's email and API key (manage bot icon → API key).
4. Subscribe the bot to whatever channel/stream holds feature topics (a
   freshly created bot isn't subscribed to anything by default).

## 2. Config

Put these in `.env` (never commit):

```
ZULIP_SITE=https://<org>.zulipchat.com
ZULIP_BOT_EMAIL=agentfleet-bot@<org>.zulipchat.com
ZULIP_BOT_API_KEY=<from step 1>
```

## 3. Local testing

Verify the bot works *before* writing any bridge code — this only needs curl
(or the official `zulip` Python package) and confirms account/permissions/
network are all correct.

Set these once in your shell (values from step 1/2):

```sh
export ZULIP_SITE=https://your-org.zulipchat.com
export ZULIP_BOT_EMAIL=agentfleet-bot@your-org.zulipchat.com
export ZULIP_BOT_API_KEY=...
```

### a. Send a message (confirms auth + channel access)

```sh
curl -sSX POST "$ZULIP_SITE/api/v1/messages" \
  -u "$ZULIP_BOT_EMAIL:$ZULIP_BOT_API_KEY" \
  --data-urlencode type=stream \
  --data-urlencode 'to=general' \
  --data-urlencode topic=agentfleet-smoke-test \
  --data-urlencode 'content=hello from a curl test'
```

A `{"result": "success", ...}` response with a message `id` means auth and
channel access both work. `STREAM_DOES_NOT_EXIST` means the channel name is
wrong or the bot isn't subscribed to it (step 1.4); `400`/`unauthorized`
means the email/API key pair is wrong.

### b. Receive replies (confirms the mechanism M3's bridge will use)

Register an event queue, narrowed to the same channel:

```sh
curl -sSX POST -u "$ZULIP_BOT_EMAIL:$ZULIP_BOT_API_KEY" \
  "$ZULIP_SITE/api/v1/register" \
  --data-urlencode 'event_types=["message"]' \
  --data-urlencode 'narrow=[["channel","general"]]'
```

Note the `queue_id` and `last_event_id` from the response, then long-poll:

```sh
curl -sSX GET -G "$ZULIP_SITE/api/v1/events" \
  -u "$ZULIP_BOT_EMAIL:$ZULIP_BOT_API_KEY" \
  --data-urlencode queue_id=<from register response> \
  --data-urlencode last_event_id=<from register response>
```

This call blocks (long-polls) until a new message arrives in that channel, or
times out. While it's blocked, post a reply from the Zulip web/mobile app (or
another terminal running step a) — the curl call should return with a
`message` event containing that content. Repeat with the returned
`last_event_id` for the next event; a `BAD_EVENT_QUEUE_ID` response means the
queue expired (`idle_queue_timeout`) and needs re-registering.

### c. Faster iteration: the official Python CLI (optional)

```sh
pip install zulip
cat > ~/.zuliprc <<EOF
[api]
key=$ZULIP_BOT_API_KEY
email=$ZULIP_BOT_EMAIL
site=$ZULIP_SITE
EOF

zulip-send --stream general --subject agentfleet-smoke-test \
  -m "hello from zulip-send"
```

`zulip-send` reads `~/.zuliprc` by default (or `--config-file`), so this is
the fastest one-liner for ad hoc sends once the account is configured — no
`curl -u` flags to repeat.

## 4. Bots (roles)

One bot per role this project addresses in Zulip (development-plan.md §6:
"address by role — requirements to the architect, implementation to the
assigned developer"):

- `orchestrator-bot` — the only bot that posts questions/approvals to humans
  once M3/M5 land (D7).
- One generic bot per additional automated poster, if needed later (e.g. a
  CI-status bot). Don't create bots speculatively — add them when a milestone
  actually needs to post as that identity.

## 5. Identity mapping

Every human and bot that will interact through Zulip needs a row in the
`identity` table (`deploy/migrations/0001_init.up.sql`): `kind`,
`display_name`, `zulip_user_id`, `github_login`, `role`. This is what lets
`af-ask-human` (M3) address messages to a role and verify a reply's sender
against a known identity — "unmapped senders are ignored and logged" (§6).

No API for this yet (`internal/api` is unimplemented until M2) — insert rows
directly via `psql` for now:

```sql
insert into identity (kind, display_name, zulip_user_id, github_login, role)
values ('human', 'Architect Name', 12345, 'gh-handle', 'architect');
```

Get a user's Zulip ID from their profile page or the `/api/v1/users` endpoint.

## 6. Topics

One topic per **feature**, not per task or per run (D4). Topic names should be
stable and human-readable — the feature slug from the `feature` table is a
reasonable default once M2 creates features programmatically.

## 7. Integration mechanism (for M3)

The bridge (`cmd/bridge`, stateless per development-plan.md §2) talks to
Zulip Cloud over its plain REST + real-time events API — no Go SDK dependency
(the available community Go Zulip clients are unmaintained/WIP; the handful
of calls needed are a thin wrapper over `net/http`, not worth an external
dependency per this repo's existing minimalism conventions):

- **Sending**: `POST /api/v1/messages` (basic auth: bot email + API key).
- **Receiving replies**: `POST /api/v1/register` once to open an event
  queue (`event_types: ["message"]`, narrowed to the relevant
  channel/topic), then long-poll `GET /api/v1/events` with the returned
  `queue_id`/`last_event_id`. No outgoing-webhook bot or public endpoint
  needed — the bridge only makes outbound HTTPS connections, consistent with
  it living on the `core` network rather than needing an inbound route from
  the internet.
- Re-register if the server returns `BAD_EVENT_QUEUE_ID` (queue expired after
  `idle_queue_timeout`).

Free-plan real limits to design around (zulip.com/plans, checked 2026-08):
10,000 messages of searchable history, 5 GB file storage, ~120 API requests/
minute per bot. None of these are binding at this team's scale.

## 8. Push notifications

Zulip Cloud runs its own push notification service — no self-hosted bouncer
registration step (`--push-notifications`, `manage.py register_server`)
needed; that requirement only applied to self-hosting.
