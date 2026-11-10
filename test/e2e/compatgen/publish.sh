#!/usr/bin/env bash
# The nightly compat job's two steps that touch git, here so that publish_test.go runs them
# as the job does, from the repository's root:
#
#   restore  starts the night from what the compat-matrix branch last published: the history,
#            the field catalog and its rendering, and the Harness Reference (docs/harnesses)
#   publish  commits the night's matrix, catalog and reference to compat-matrix and pushes it
#   publish-reference
#            commits the reference alone (the frequent check's new builds, reference.yml),
#            leaving the night's matrix and catalog as they are
#   restore-models
#            puts back the Model Reference (docs/models) as last published, and the Harness
#            Reference it reads
#   publish-models
#            commits the Model Reference alone (models.yml), leaving the rest as it is
#
# What they write is generated, and git-ignored on every other branch.
set -euo pipefail

branch=compat-matrix

case "${1:-}" in
restore)
	git fetch -q origin "$branch" 2>/dev/null || exit 0
	mkdir -p docs/compat
	for f in docs/compat/history.json docs/compat/fields.json docs/compat/compat.json docs/FIELDS.md; do
		git show "origin/$branch:$f" >"$f" 2>/dev/null || rm -f "$f"
	done
	# The reference's snapshots are kept from night to night: a night merges into them.
	rm -rf docs/harnesses
	if git cat-file -e "origin/$branch:docs/harnesses" 2>/dev/null; then
		git archive "origin/$branch" docs/harnesses | tar -x
	fi
	;;
publish)
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	cp -R docs/COMPATIBILITY.md docs/compat "$tmp/"
	if [ -f docs/FIELDS.md ]; then cp docs/FIELDS.md "$tmp/"; fi
	if [ -d docs/harnesses ]; then cp -R docs/harnesses "$tmp/"; fi
	# The checkout below must not meet them: it overwrites an ignored file, but refuses to
	# overwrite an untracked one, and every night restores some.
	rm -rf docs/COMPATIBILITY.md docs/FIELDS.md docs/compat docs/harnesses
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
	# Wholly the night's: a file refgen no longer writes (a diff between builds no longer
	# neighbours) leaves the branch with it.
	rm -rf docs/harnesses
	if [ -d "$tmp/harnesses" ]; then cp -R "$tmp/harnesses" docs/harnesses; fi
	git add -f docs/COMPATIBILITY.md docs/compat
	if [ -f docs/FIELDS.md ]; then git add -f docs/FIELDS.md; fi
	if [ -d docs/harnesses ]; then git add -A -f docs/harnesses; fi
	if git diff --cached --quiet; then exit 0; fi
	git commit -q -m "compat: nightly matrix, field catalog and harness reference, $(date -u +%F)"
	git push -q origin "$branch"
	;;
publish-reference)
	[ -d docs/harnesses ] || exit 0
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	cp -R docs/harnesses "$tmp/"
	# What restore put back must not meet the checkout either.
	rm -rf docs/COMPATIBILITY.md docs/FIELDS.md docs/compat docs/harnesses
	git config user.name "terma-compat"
	git config user.email "terma-compat@users.noreply.github.com"
	if git rev-parse -q --verify "origin/$branch" >/dev/null; then
		git checkout -q -B "$branch" "origin/$branch"
	else
		git checkout -q --orphan "$branch"
		git rm -rq --cached . && git clean -fdxq
	fi
	mkdir -p docs
	rm -rf docs/harnesses
	cp -R "$tmp/harnesses" docs/harnesses
	git add -A -f docs/harnesses
	if git diff --cached --quiet; then exit 0; fi
	git commit -q -m "reference: ${TERMA_REFERENCE_BUILDS:-new builds}, $(date -u +%FT%RZ)"
	git push -q origin "$branch"
	;;
restore-models)
	# The Model Reference merges into what it last published, and reads the Harness Reference
	# for the harnesses that use each model.
	git fetch -q origin "$branch" 2>/dev/null || exit 0
	rm -rf docs/models docs/harnesses
	for d in docs/models docs/harnesses; do
		if git cat-file -e "origin/$branch:$d" 2>/dev/null; then
			git archive "origin/$branch" "$d" | tar -x
		fi
	done
	;;
publish-models)
	[ -d docs/models ] || exit 0
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	cp -R docs/models "$tmp/"
	rm -rf docs/models docs/harnesses
	git config user.name "terma-compat"
	git config user.email "terma-compat@users.noreply.github.com"
	if git rev-parse -q --verify "origin/$branch" >/dev/null; then
		git checkout -q -B "$branch" "origin/$branch"
	else
		git checkout -q --orphan "$branch"
		git rm -rq --cached . && git clean -fdxq
	fi
	mkdir -p docs
	rm -rf docs/models
	cp -R "$tmp/models" docs/models
	git add -A -f docs/models
	if git diff --cached --quiet; then exit 0; fi
	git commit -q -m "models: ${TERMA_MODELS_SUMMARY:-changes}, $(date -u +%FT%RZ)"
	git push -q origin "$branch"
	;;
*)
	echo "usage: $0 restore|publish|publish-reference|restore-models|publish-models" >&2
	exit 2
	;;
esac
