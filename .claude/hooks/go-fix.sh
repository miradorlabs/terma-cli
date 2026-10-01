#!/usr/bin/env bash
# PostToolUse: `go fix` modernizers on each edited Go package (make format runs it repo-wide).
# Run from the package's own directory: test/live/ is a module of its own, with no
# go.work. Best-effort: a package that does not compile, or no `go`, is a silent no-op.

set -u
command -v go >/dev/null 2>&1 || exit 0
. "$(dirname "$0")/lib.sh"

go_package_dirs "$(edited_files "$(cat)")" | while read -r _ d; do
  ( cd "$d" && go fix . >/dev/null 2>&1 ) || true
done
exit 0
