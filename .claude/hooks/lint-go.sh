#!/usr/bin/env bash
# PostToolUse: golangci-lint each edited Go package and feed findings back to Claude (exit 2).
# The linter is the one `make lint` builds — the version CI runs, compiled with this Go; a
# released binary refuses a module whose `go` directive is newer than its own. Until
# `make lint` has run once there is nothing to run, so it says nothing.

set -u
. "$(dirname "$0")/lib.sh"

status=0
while read -r root d; do
  lint="$root/bin/golangci-lint-$(cat "$root/.golangci-lint-version" 2>/dev/null)"
  [ -x "$lint" ] || continue
  # From the package's own directory, so a nested module (live/, pocs/*) lints as itself.
  out="$(cd "$d" && "$lint" run . 2>&1)" && continue
  printf '%s\n' "$out" >&2
  status=2
done <<< "$(go_package_dirs "$(edited_files "$(cat)")")"
exit "$status"
