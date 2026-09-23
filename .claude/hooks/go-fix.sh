#!/usr/bin/env bash
# PostToolUse hook: apply `go fix` modernizers to the package(s) of edited Go files.
# Scoped to each edited file's package (fast, ~0.3s) — full-module `go fix ./...`
# lives in `make format`, and was removed from `make build` to keep builds/integration
# tests quick. Registered to run BEFORE format-go.sh so goimports/gofmt reconcile any
# imports the modernizers add (e.g. slices, maps).
#
# Tool input shapes handled:
#   Edit / Write       → tool_input.file_path
#   MultiEdit          → tool_input.file_path (single-file form), and/or
#                        tool_input.edits[].file_path (cross-file form)
#
# Parser: tries jq first, falls back to python3, no-ops if neither exists.
# Best-effort: a non-compiling package or missing `go` is a silent no-op.

set -u

command -v go >/dev/null 2>&1 || exit 0

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

# Reduce edited .go files to their package directories — go fix is
# package-scoped, so one run per directory covers every edited file in it.
dirs=""
while IFS= read -r p; do
  [ -z "$p" ] && continue
  [ ! -f "$p" ] && continue
  case "$p" in
    *.go) dirs="${dirs}$(dirname "$p")"$'\n' ;;
  esac
done <<< "$paths"

# Deduplicate directories (multiple edited files in one package ⇒ fix once).
dirs="$(printf '%s' "$dirs" | awk 'NF && !seen[$0]++')"
[ -z "$dirs" ] && exit 0

while IFS= read -r d; do
  [ -z "$d" ] && continue
  # Run from the edited package's own directory, not $CLAUDE_PROJECT_DIR: this repo has
  # nested modules (live/, pocs/*) with no go.work, so `go fix <root-relative pkg>` from
  # the root module cannot resolve them. Running from the package dir selects its
  # owning module.
  ( cd "$d" && go fix . >/dev/null 2>&1 ) || true
done <<< "$dirs"

exit 0
