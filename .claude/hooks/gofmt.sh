#!/bin/bash
# PostToolUse hook: chạy gofmt trên file .go vừa được Edit/Write. Không bao giờ chặn tool call.
set -uo pipefail

file="$(jq -r '.tool_input.file_path // empty' 2>/dev/null)"
case "$file" in
  *.go)
    if [ -f "$file" ] && command -v gofmt >/dev/null 2>&1; then
      gofmt -w "$file" 2>&1 || true
    fi
    ;;
esac
exit 0
