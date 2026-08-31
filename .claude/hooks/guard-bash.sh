#!/usr/bin/env bash
cmd="$(jq -r '.tool_input.command // empty')"
if echo "$cmd" | grep -Eq 'rm[[:space:]]+-rf|git[[:space:]]+push[[:space:]]+--force|:\(\)\{'; then
  echo "Blocked dangerous command: $cmd" >&2
  exit 2
fi
exit 0
