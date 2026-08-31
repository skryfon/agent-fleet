#!/usr/bin/env bash
# Blocks Write/Edit/Read on sensitive files. Reads hook JSON on stdin.
path="$(jq -r '.tool_input.file_path // empty')"
case "$path" in
  *.env|*.env.*|*/.env|*.pem|*id_rsa*|*secrets*|*credentials*)
    echo "Blocked: $path is a protected/secret file." >&2
    exit 2 ;;
esac
exit 0
