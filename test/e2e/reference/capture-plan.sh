#!/usr/bin/env bash
# The frequent check's capture jobs (reference.yml's detect job), from the builds to capture, one
# line a harness ("harness=version,…", new-builds.sh's or a run's input) on stdin. Writes the
# job's outputs to the file it is given (GITHUB_OUTPUT): each harness's versions, under its id
# with dashes as underscores; matrix, one capture job a command-line harness
# ({"include":[{"harness","versions"}]}); desktop_tests, the profile scenarios of the desktop
# apps; builds, every line taken. Prints the lines it takes: a line that names no harness and
# versions (each starting with a digit: no "latest", no "last1") is left out, so nothing of an
# input reaches a job's name or a path unchecked, nor names a page after a word.
set -euo pipefail

out=$1
version='[0-9][0-9A-Za-z.+_-]*'
builds=$(grep -E "^[a-z][a-z0-9-]*=$version(,$version)*\$" || true)
[ -n "$builds" ] || exit 0
include='[]'
desktop=()
while IFS='=' read -r harness versions; do
	echo "${harness//-/_}=$versions" >>"$out"
	case "$harness" in
	claude-desktop) desktop+=(TestClaudeDesktopProfile) ;;
	codex-desktop) desktop+=(TestCodexDesktopProfile) ;;
	*) include=$(jq -c --arg h "$harness" --arg v "$versions" '. + [{harness: $h, versions: $v}]' <<<"$include") ;;
	esac
done <<<"$builds"
if [ "$include" != '[]' ]; then
	echo "matrix=$(jq -c '{include: .}' <<<"$include")" >>"$out"
fi
if [ ${#desktop[@]} -gt 0 ]; then
	echo "desktop_tests=^($(
		IFS='|'
		echo "${desktop[*]}"
	))\$" >>"$out"
fi
{
	echo 'builds<<EOF'
	echo "$builds"
	echo 'EOF'
} >>"$out"
echo "$builds"
