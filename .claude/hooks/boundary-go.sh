#!/usr/bin/env bash
# PostToolUse: run internal/boundary (the architecture as tests, docs/ARCHITECTURE.md) after a
# Go edit in the main module and feed violations back to Claude (exit 2).

set -u
command -v go >/dev/null 2>&1 || exit 0
. "$(dirname "$0")/lib.sh"

root="$(go_package_dirs "$(edited_files "$(cat)")" | awk 'NR == 1 { print $1 }')"
[ -n "$root" ] && [ -d "$root/internal/boundary" ] || exit 0

out="$(cd "$root" && TERMA_ENV=dev go test -count=1 ./internal/boundary/ 2>&1)" && exit 0
printf '%s\n' "$out" | grep -E '_test.go:[0-9]+:' | sed -E 's/^[[:space:]]*[a-z_]+_test.go:[0-9]+: //' >&2
echo "Fix the layering per docs/ARCHITECTURE.md, 'What the tests enforce'." >&2
exit 2
