#!/usr/bin/env bash
# The reference scenarios (each harness's profile and probes) of the harnesses the frequent
# check captures (new-builds.sh --tracked): it captures each of their builds once, fully, so the
# night skips them, and captures only the harnesses it does not watch.
#
#   scenarios.sh skip        a -test.skip pattern of the watched harnesses' reference scenarios
#   scenarios.sh unwatched   the harnesses of the shared probes (TestHarnessProbe*) not watched,
#                            comma-separated, for TERMA_E2E_HARNESSES
#   scenarios.sh drivers     every harness of the shared probes (probes_harnesses_test.go)
set -euo pipefail

# drivers are probes_harnesses_test.go's harnessDrivers, which TestScenariosListsEveryDriver
# keeps this list in step with.
drivers=(gemini opencode pi omp hermes dsh copilot)

dir=$(cd "$(dirname "$0")" && pwd)
watched=$("$dir/new-builds.sh" --tracked)

# testName is how a harness's scenarios are named: TestOpenCodeProfile, TestDshProbeHooks.
testName() {
	case "$1" in
	opencode) echo OpenCode ;;
	*) echo "$(tr '[:lower:]' '[:upper:]' <<<"${1:0:1}")${1:1}" ;;
	esac
}

case "${1:-}" in
skip)
	names=()
	while read -r h; do
		case "$h" in *-desktop) continue ;; esac # the desktop apps' scenarios run only where asked for
		names+=("$(testName "$h")")
	done <<<"$watched"
	echo "^Test($(IFS='|'; echo "${names[*]}"))(Profile|Probe)"
	;;
unwatched)
	out=()
	for d in "${drivers[@]}"; do
		grep -qxF "$d" <<<"$watched" || out+=("$d")
	done
	(IFS=','; echo "${out[*]}")
	;;
drivers)
	printf '%s\n' "${drivers[@]}"
	;;
*)
	echo "usage: $0 skip|unwatched|drivers" >&2
	exit 2
	;;
esac
