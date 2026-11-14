#!/usr/bin/env bash
# What refgen is told of a run's captures (reference.yml's publish job), one argument a line:
# -attempted for every build the run set out to capture, and -unfinished for those a job of it
# did not finish. A capture job's run writes compat.json when it gets to its end (TestMain), and
# the job then says so in capture.json ({"job": "macos" or "linux", "harness", "finished"}): a job
# whose run did not end (a timeout, a runner lost) has its builds published partial, and tried
# again some hours later (new-builds.sh). The desktop job runs every app's scenario, so an app is
# judged by its own profile: one it recorded finished, whatever the other app did (a scenario
# that recorded and failed leaves the build partial in refgen all the same).
#
# Every file is found by what it says, not by the folder it sits in: download-artifact unpacks
# each artifact a pattern matches into a folder of its name, but the only one it matches into
# <runs> itself.
#
# capture-args.sh <runs>, where <runs> holds the jobs' artifacts; BUILDS is the detect job's lines
# (harness=version,…), CLAUDE_BUILDS and CODEX_BUILDS what the Linux job captured,
# DESKTOP_CAPTURE the desktop job's result.
set -euo pipefail

runs=$1

# all is every file named $1 under <runs>, read as one JSON array of their contents.
all() { find "$runs" -name "$1" -exec cat {} + 2>/dev/null | jq -s . ; }

# finished reports that a job's marker says its run got to its end: the macOS job of harness $2,
# or the Linux job ($1 linux).
finished() {
	all capture.json | jq -e --arg job "$1" --arg harness "${2:-}" \
		'any(.[]; .job == $job and ($harness == "" or .harness == $harness) and .finished == true)' >/dev/null
}

# unrecorded prints, comma-separated, the versions of harness no profile was recorded of.
unrecorded() {
	local harness=$1 versions=$2 v missing="" profiles
	profiles=$(all profiles.json)
	for v in ${versions//,/ }; do
		if ! jq -e --arg h "$harness" --arg v "$v" 'any(add // [] | .[]; .harness == $h and .version == $v)' <<<"$profiles" >/dev/null; then
			missing="${missing:+$missing,}$v"
		fi
	done
	echo "$missing"
}

while IFS= read -r line; do
	[ -n "$line" ] || continue
	harness=${line%%=*}
	printf '%s\n' -attempted "$line"
	case "$harness" in
	claude-desktop | codex-desktop)
		if [ "${DESKTOP_CAPTURE:-}" != success ]; then
			missing=$(unrecorded "$harness" "${line#*=}")
			if [ -n "$missing" ]; then printf '%s\n' -unfinished "$harness=$missing"; fi
		fi
		;;
	*) finished macos "$harness" || printf '%s\n' -unfinished "$line" ;;
	esac
done <<<"${BUILDS:-}"
if ! finished linux; then
	if [ -n "${CLAUDE_BUILDS:-}" ]; then printf '%s\n' -unfinished "claude=$CLAUDE_BUILDS"; fi
	if [ -n "${CODEX_BUILDS:-}" ]; then printf '%s\n' -unfinished "codex=$CODEX_BUILDS"; fi
fi
