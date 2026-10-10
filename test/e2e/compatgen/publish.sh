#!/usr/bin/env bash
# The nightly compat job's two steps that touch git, here so that publish_test.go runs them
# as the job does, from the repository's root:
#
#   restore  starts the night from what the compat-matrix branch last published: the history,
#            the field catalog and its rendering
#   publish  commits the night's matrix and catalog to compat-matrix and pushes it
#
# What they write is generated, and git-ignored on every other branch.
set -euo pipefail

branch=compat-matrix

case "${1:-}" in
restore)
	git fetch -q origin "$branch" 2>/dev/null || exit 0
	mkdir -p docs/compat
	for f in docs/compat/history.json docs/compat/fields.json docs/FIELDS.md; do
		git show "origin/$branch:$f" >"$f" 2>/dev/null || rm -f "$f"
	done
	;;
publish)
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	cp -R docs/COMPATIBILITY.md docs/compat "$tmp/"
	if [ -f docs/FIELDS.md ]; then cp docs/FIELDS.md "$tmp/"; fi
	# The checkout below must not meet them: it overwrites an ignored file, but refuses to
	# overwrite an untracked one, and every night restores some.
	rm -rf docs/COMPATIBILITY.md docs/FIELDS.md docs/compat
	git config user.name "terma-compat"
	git config user.email "terma-compat@users.noreply.github.com"
	if git rev-parse -q --verify "origin/$branch" >/dev/null; then
		git checkout -q -B "$branch" "origin/$branch"
	else
		git checkout -q --orphan "$branch"
		git rm -rq --cached . && git clean -fdxq
	fi
	mkdir -p docs
	rm -rf docs/compat
	cp "$tmp/COMPATIBILITY.md" docs/COMPATIBILITY.md
	cp -R "$tmp/compat" docs/compat
	if [ -f "$tmp/FIELDS.md" ]; then cp "$tmp/FIELDS.md" docs/FIELDS.md; fi
	git add -f docs/COMPATIBILITY.md docs/compat
	if [ -f docs/FIELDS.md ]; then git add -f docs/FIELDS.md; fi
	if git diff --cached --quiet; then exit 0; fi
	git commit -q -m "compat: nightly matrix and field catalog, $(date -u +%F)"
	git push -q origin "$branch"
	;;
*)
	echo "usage: $0 restore|publish" >&2
	exit 2
	;;
esac
