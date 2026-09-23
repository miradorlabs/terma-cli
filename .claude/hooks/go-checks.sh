#!/usr/bin/env bash
# PostToolUse orchestrator: run the Go edit-time checks in the one order they depend
# on — go fix, then format, then lint.
#
# Claude Code runs every command hook in a matcher array CONCURRENTLY, not in array
# order (https://code.claude.com/docs/en/hooks). Registering go-fix/format/lint as
# three sibling entries therefore races: `go fix` and `goimports` rewrite the same
# file at once (lost edits), and `golangci-lint` can read a package mid-rewrite
# (spurious failures). Chaining them here fixes that — go fix adds imports that
# gofmt/goimports must reconcile before lint runs.
#
# The hook payload arrives once on stdin, and each script reads all of it, so it is
# captured here and replayed to each. go-fix and format-go are best-effort (they exit
# 0); lint-go's exit status is meaningful and, as the last command, becomes this
# script's — so a lint failure still surfaces to Claude.

set -u

dir="$(dirname "$0")"
input="$(cat)"

printf '%s' "$input" | "$dir/go-fix.sh"
printf '%s' "$input" | "$dir/format-go.sh"
printf '%s' "$input" | "$dir/lint-go.sh"
