#!/usr/bin/env bash
# PostToolUse hook: lint packages containing edited Go files after fix + format.

set -u

# The linter `make lint` builds: the version CI runs, compiled with this machine's Go. A
# golangci-lint from PATH is not a substitute — a released binary refuses a module whose
# `go` directive is newer than the Go it was built with, and this hook then failed on
# every edit. Until `make lint` has been run once there is nothing to run, so say nothing.
root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$root" ] || exit 0
lint="$root/bin/golangci-lint-$(cat "$root/.golangci-lint-version" 2>/dev/null)"
[ -x "$lint" ] || exit 0

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

dirs=""
while IFS= read -r p; do
  [ -z "$p" ] && continue
  [ ! -f "$p" ] && continue
  case "$p" in
    *.go) dirs="${dirs}$(dirname "$p")"$'\n' ;;
  esac
done <<< "$paths"

dirs="$(printf '%s' "$dirs" | awk 'NF && !seen[$0]++')"
[ -z "$dirs" ] && exit 0

status=0
while IFS= read -r d; do
  [ -z "$d" ] && continue
  # Run from the edited package's own directory, not $CLAUDE_PROJECT_DIR: this repo has
  # nested modules (live/, pocs/*) with no go.work, and linting them from the root
  # module fails with "main module ... does not contain package ...". Running from the
  # package dir selects its owning module; golangci-lint walks up to the root
  # .golangci.yml either way.
  ( cd "$d" && "$lint" run . ) || status=1
done <<< "$dirs"

exit "$status"
