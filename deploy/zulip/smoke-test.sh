#!/usr/bin/env bash
# M3 manual smoke test: drives the ask_human -> Zulip -> reply -> resume
# round trip against a real control plane + bridge + Zulip Cloud org.
#
# Two modes:
#   fast   Inserts a task/run directly via psql (no container, no LLM) and
#          exercises ask_human -> Zulip post -> your reply -> answer
#          recorded, over the real HTTP API. Proves the M3 bridge mechanics
#          in isolation.
#   full   Ingests a real task instructing the agent to call ask_human,
#          starts it, and drives a real runner container through the
#          checkpoint-and-exit / resurrect-and-resume path. Needs
#          `make runner-image` already built.
#
# Prerequisites: `make up` running, .env filled in (see deploy/zulip/README.md
# and the M3 section of that file for ZULIP_STREAM/BRIDGE_SECRET), jq, psql,
# curl, an identity row mapping your Zulip user id (see step 3 below).
#
# Usage:
#   deploy/zulip/smoke-test.sh fast
#   deploy/zulip/smoke-test.sh full
set -euo pipefail

MODE="${1:-}"
if [ "$MODE" != "fast" ] && [ "$MODE" != "full" ]; then
  echo "usage: $0 <fast|full>" >&2
  exit 1
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENV_FILE="$ROOT/.env"

if [ ! -f "$ENV_FILE" ]; then
  echo "missing $ENV_FILE — copy .env.example and fill it in first" >&2
  exit 1
fi

set -a
# shellcheck source=/dev/null
source "$ENV_FILE"
set +a

for var in ADMIN_TOKEN DATABASE_URL ZULIP_STREAM; do
  if [ -z "${!var:-}" ]; then
    echo "missing $var in .env" >&2
    exit 1
  fi
done

for bin in jq psql curl; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin" >&2; exit 1; }
done

BASE="${CONTROL_PLANE_BASE_URL:-http://localhost:8080}"
SUFFIX="$(date +%s)"

log() { echo "-- $*" >&2; }

api() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -X "$method" "$BASE$path" -H "Authorization: Bearer $ADMIN_TOKEN" -d "$body"
  else
    curl -sS -X "$method" "$BASE$path" -H "Authorization: Bearer $ADMIN_TOKEN"
  fi
}

log "creating project/feature (suffix $SUFFIX)"

PROJ_SLUG="m3-smoke-$SUFFIX"
FEAT_SLUG="m3-smoke-$SUFFIX"

PROJ=$(api POST /v1/projects "$(jq -n --arg slug "$PROJ_SLUG" '{
  slug: $slug, manifest_ref: "main", manifest_hash: "deadbeef",
  repos: ["https://github.com/octocat/Hello-World.git"]
}')")
PROJ_SLUG_RET=$(echo "$PROJ" | jq -r '.slug')

FEAT=$(api POST "/v1/projects/$PROJ_SLUG_RET/features" "$(jq -n --arg slug "$FEAT_SLUG" --arg topic "$FEAT_SLUG" '{
  slug: $slug, zulip_topic: $topic
}')")
FEAT_ID=$(echo "$FEAT" | jq -r '.id')

log "feature id: $FEAT_ID — watch topic \"$FEAT_SLUG\" in stream \"$ZULIP_STREAM\""

wait_for_answer() {
  local question_id="$1"
  log "waiting for the question to be ANSWERED (reply in Zulip topic \"$FEAT_SLUG\" now)"
  log "question_id: $question_id"

  while true; do
    STATE=$(psql "$DATABASE_URL" -tA -c "select state from question where id = '$question_id';")
    if [ "$STATE" = "ANSWERED" ]; then
      log "answered:"
      psql "$DATABASE_URL" -c "select answer, answered_by, answered_at from question where id = '$question_id';"
      return 0
    fi
    sleep 3
  done
}

if [ "$MODE" = "fast" ]; then
  log "fast mode: inserting task+run directly (no container, no LLM)"

  TASK_ID=$(psql "$DATABASE_URL" -tAq -c "
    insert into task (feature_id, lane, title, intent, acceptance_criteria, state)
    values ('$FEAT_ID', 'direct', 'M3 smoke test', 'nothing to build', '[]', 'RUNNING')
    returning id;")

  RUN_TOKEN="smoke-token-$SUFFIX"
  TOKEN_HEX=$(printf '%s' "$RUN_TOKEN" | shasum -a 256 | awk '{print $1}')

  RUN_ID=$(psql "$DATABASE_URL" -tAq -c "
    insert into run (id, task_id, role, model, state, token_hash)
    values (gen_random_uuid(), '$TASK_ID', 'orchestrator', 'test', 'RUNNING', decode('$TOKEN_HEX', 'hex'))
    returning id;")

  log "task: $TASK_ID  run: $RUN_ID"

  ASK=$(curl -sS -X POST "$BASE/v1/runs/$RUN_ID/tools/ask_human" \
    -H "Authorization: Bearer $RUN_TOKEN" \
    -d '{"question":"which branch should this target?","kind":"free_text"}')
  echo "$ASK" | jq .

  ALLOW=$(echo "$ASK" | jq -r '.allow')
  if [ "$ALLOW" != "true" ]; then
    log "ask_human was denied — check the manifest grants ask_human to the run's role"
    exit 1
  fi

  QUESTION_ID=$(echo "$ASK" | jq -r '.result.question_id')

  log "task state after ask (should be BLOCKED_ON_HUMAN):"
  psql "$DATABASE_URL" -c "select state from task where id = '$TASK_ID';"

  log "second concurrent ask (should 409):"
  curl -sS -o /dev/null -w "status: %{http_code}\n" -X POST "$BASE/v1/runs/$RUN_ID/tools/ask_human" \
    -H "Authorization: Bearer $RUN_TOKEN" \
    -d '{"question":"another one?","kind":"free_text"}'

  wait_for_answer "$QUESTION_ID"

  log "task state after answer (should be RUNNING):"
  psql "$DATABASE_URL" -c "select state from task where id = '$TASK_ID';"

  log "fast mode done."
  exit 0
fi

# full mode
log "full mode: ingesting a real task and starting it — needs make runner-image already built"

TASKS_MD=$'```yaml agentfleet-tasks\nversion: v1\ntasks:\n  - external_ref: T1\n    lane: direct\n    title: M3 full-flow test\n    intent: >\n      Call the ask_human tool with kind=confirm and question\n      "Should I proceed?" before doing anything else. Do nothing\n      further until you receive an answer.\n```\n'

INGESTED=$(curl -sS -X POST "$BASE/v1/features/$FEAT_ID/tasks:ingest" \
  -H "Authorization: Bearer $ADMIN_TOKEN" --data-binary "$TASKS_MD")
echo "$INGESTED" | jq .

TASK_ID=$(echo "$INGESTED" | jq -r '.[0].id')
log "task id: $TASK_ID"

api POST "/v1/tasks/$TASK_ID/start" | jq .

log "watch the supervisor logs in another terminal:"
log "  podman compose -f deploy/compose.yaml logs -f supervisor"
log "waiting for the task to reach BLOCKED_ON_HUMAN (agent called ask_human)..."

while true; do
  STATE=$(psql "$DATABASE_URL" -tA -c "select state from task where id = '$TASK_ID';")
  [ "$STATE" = "BLOCKED_ON_HUMAN" ] && break
  sleep 5
done

log "task is BLOCKED_ON_HUMAN. finding the open question..."
QUESTION_ID=$(psql "$DATABASE_URL" -tA -c "
  select id from question where task_id = '$TASK_ID' and state = 'OPEN' order by asked_at desc limit 1;")

log "question_id: $QUESTION_ID"
log "waiting for the run to checkpoint-and-exit (up to 5 minutes)..."

while true; do
  RUN_STATE=$(psql "$DATABASE_URL" -tA -c "
    select state from run where task_id = '$TASK_ID' order by created_at desc limit 1;")
  case "$RUN_STATE" in
    EXITED|FAILED) break ;;
  esac
  sleep 5
done

log "run reached $RUN_STATE. reply in Zulip topic \"$FEAT_SLUG\" now."

wait_for_answer "$QUESTION_ID"

log "watch the supervisor logs for a NEW container launching (resume), reusing agentfleet-task-$TASK_ID"
log "full mode: everything from here is watch-and-verify — see the conversation for what to check."
