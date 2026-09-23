#!/usr/bin/env bash
# PostToolUse hook: auto-format Go and proto files after edits.
# Mirrors the "make format" rule in CLAUDE.md so it's enforced, not advisory.
#
# Tool input shapes handled:
#   Edit / Write       → tool_input.file_path
#   MultiEdit          → tool_input.file_path (single-file form), and/or
#                        tool_input.edits[].file_path (cross-file form)
#
# Parser: tries jq first, falls back to python3, no-ops if neither exists
# (formatting is best-effort; a missing parser is not a security issue here).

set -u

input="$(cat)"

extract_paths() {
  local out
  if command -v jq >/dev/null 2>&1; then
    if out=$(printf '%s' "$1" | jq -r '
      [
        .tool_input.file_path?,
        ( .tool_input.edits? // [] | .[]?.file_path? )
      ]
      | map(select(. != null and . != ""))
      | .[]
    ' 2>/dev/null); then
      printf '%s' "$out"
      return 0
    fi
  fi
  if command -v python3 >/dev/null 2>&1; then
    if out=$(printf '%s' "$1" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
    ti = d.get("tool_input", {}) or {}
    fp = ti.get("file_path")
    if fp:
        print(fp)
    for e in (ti.get("edits") or []):
        f = (e or {}).get("file_path")
        if f:
            print(f)
except Exception:
    pass
' 2>/dev/null); then
      printf '%s' "$out"
      return 0
    fi
  fi
  return 1
}

paths="$(extract_paths "$input")" || exit 0
[ -z "$paths" ] && exit 0

# Deduplicate paths (multi-edit on the same file ⇒ format once).
paths="$(printf '%s\n' "$paths" | awk 'NF && !seen[$0]++')"

while IFS= read -r p; do
  [ -z "$p" ] && continue
  [ ! -f "$p" ] && continue
  case "$p" in
    *.go)
      # goimports is a superset of gofmt — only fall back to gofmt if missing.
      if command -v goimports >/dev/null 2>&1; then
        goimports -w "$p" >/dev/null 2>&1 || true
      elif command -v gofmt >/dev/null 2>&1; then
        gofmt -w "$p" >/dev/null 2>&1 || true
      fi
      ;;
    *.proto)
      if command -v buf >/dev/null 2>&1; then
        ( cd "$CLAUDE_PROJECT_DIR" && buf format -w "$p" >/dev/null 2>&1 ) || true
      fi
      ;;
  esac
done <<< "$paths"

exit 0
