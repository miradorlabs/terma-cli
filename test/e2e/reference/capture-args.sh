#!/usr/bin/env bash
# What refgen is told of a run's captures (reference.yml's publish job), one argument a line:
# -attempted for every build the run set out to capture, and -unfinished for those a job of it
# did not finish. A capture job's run writes compat.json when it gets to its end (TestMain), so a
# job without one did not finish (a timeout, a runner lost): its builds are published partial, and
# tried again some hours later (new-builds.sh). The desktop job runs every app's scenario, so an
# app is judged by its own profile: one it recorded finished, whatever the other app did (a
# scenario that recorded and failed leaves the build partial in refgen all the same).
#
# capture-args.sh <runs>, where <runs> holds the jobs' artifacts, each in a folder of its name;
# BUILDS is the detect job's lines (harness=version,…), CLAUDE_BUILDS and CODEX_BUILDS what the
# Linux job captured, DESKTOP_CAPTURE the desktop job's result.
set -euo pipefail

runs=$1

# recorded reports that the desktop job recorded a profile of every version of harness listed.
recorded() {
	local harness=$1 versions=$2 v
	[ -f "$runs/profiles-desktop/profiles.json" ] || return 1
	for v in ${versions//,/ }; do
		jq -e --arg h "$harness" --arg v "$v" 'any(.[]; .harness == $h and .version == $v)' \
			"$runs/profiles-desktop/profiles.json" >/dev/null || return 1
	done
}

while IFS= read -r line; do
	[ -n "$line" ] || continue
	harness=${line%%=*}
	printf '%s\n' -attempted "$line"
	case "$harness" in
	claude-desktop | codex-desktop) [ "${DESKTOP_CAPTURE:-}" = success ] || recorded "$harness" "${line#*=}" || printf '%s\n' -unfinished "$line" ;;
	*) [ -f "$runs/capture-$harness/compat.json" ] || printf '%s\n' -unfinished "$line" ;;
	esac
done <<<"${BUILDS:-}"
if [ ! -f "$runs/compat-linux/compat.json" ]; then
	if [ -n "${CLAUDE_BUILDS:-}" ]; then printf '%s\n' -unfinished "claude=$CLAUDE_BUILDS"; fi
	if [ -n "${CODEX_BUILDS:-}" ]; then printf '%s\n' -unfinished "codex=$CODEX_BUILDS"; fi
fi
