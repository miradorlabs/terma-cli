#!/usr/bin/env bash
# PostToolUse: format edited Go (goimports, else gofmt) and proto (buf) files, as `make format` does.
# Best-effort: a missing formatter is a no-op.

set -u
. "$(dirname "$0")/lib.sh"

edited_files "$(cat)" | while IFS= read -r p; do
  case "$p" in
    *.go)
      if command -v goimports >/dev/null 2>&1; then
        goimports -w "$p" >/dev/null 2>&1 || true
      elif command -v gofmt >/dev/null 2>&1; then
        gofmt -w "$p" >/dev/null 2>&1 || true
      fi
      ;;
    *.proto)
      root="$(git -C "$(dirname "$p")" rev-parse --show-toplevel 2>/dev/null)" || continue
      if command -v buf >/dev/null 2>&1; then
        ( cd "$root" && buf format -w "$p" >/dev/null 2>&1 ) || true
      fi
      ;;
  esac
done
exit 0
