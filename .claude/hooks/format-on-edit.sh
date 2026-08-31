#!/usr/bin/env bash
path="$(jq -r '.tool_input.file_path // empty')"
[ -z "$path" ] && exit 0
case "$path" in
  *.go) gofmt -w "$path" 2>/dev/null ;;
  *.js|*.ts|*.jsx|*.tsx|*.json|*.css|*.md|*.yaml|*.yml) npx --no-install prettier --write "$path" 2>/dev/null ;;
esac
exit 0
