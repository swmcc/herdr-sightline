#!/usr/bin/env bash
# Claude Code PostToolUse hook. Formats Go files and checks the prose in Markdown files
# straight after they're edited. Exit code 2 sends the message back to Claude to fix.
set -euo pipefail

file=$(jq -r '.tool_input.file_path // empty')
[ -n "$file" ] && [ -f "$file" ] || exit 0

case "$file" in
  *.go)
    command -v gofmt >/dev/null && gofmt -w "$file"
    ;;
  *.md)
    if ! out=$("$CLAUDE_PROJECT_DIR/.claude/scripts/check-prose.sh" "$file" 2>&1); then
      printf '%s\n' "$out" >&2
      exit 2
    fi
    [ -n "$out" ] && printf '%s\n' "$out" >&2 && exit 2
    ;;
esac
exit 0
