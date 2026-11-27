#!/bin/sh
# Splits the e2e tests that match a -run pattern into CI shards and prints the -run pattern
# for one of them: `e2e-shard.sh <shard> '<pattern>'`. `e2e-shard.sh list '<pattern>'` prints
# every test with the shard it runs in. A test runs in the first shard whose pattern matches
# its name; `others` takes the rest, so a new test always runs somewhere. The shards are
# balanced by the times CI measured (about four to five minutes each).
#
# OpenCode's tests run twice, once per major: opencode-1 and opencode-2 are the same tests,
# and CI gives each shard its own builds (TERMA_E2E_OPENCODE_VERSIONS).
set -eu
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SHARDS="claude codex-a codex-b copilot opencode others"

[ $# -eq 2 ] || { echo "usage: $0 <claude|codex-a|codex-b|copilot|opencode-1|opencode-2|others|list> '<go test -run pattern>'" >&2; exit 2; }
case "$1" in opencode-1 | opencode-2) set -- opencode "$2" ;; esac

pattern() {
	case "$1" in
	# Claude Code's workloads, desktop and restart runs, and T3 Code, which drives it.
	claude) echo 'Claude|^TestRelayT3$' ;;
	# Codex's slowest: its workloads, the long turn, the daemon and the desktop app.
	codex-a) echo '^Test(RelayWorkloadsCodex|RelayCodexLongTurn|RelayCodexDaemonTUI|RelayCodexDesktop.*|CodexTelemetry)$' ;;
	# The rest of Codex, DeepSeek Harness, Pi, OMP and Grok Build.
	codex-b) echo 'Codex|Dsh|Pi([A-Z]|$)|Omp|Grok' ;;
	# GitHub Copilot, which runs against GitHub's service: its own check.
	copilot) echo 'Copilot' ;;
	# OpenCode, on its oldest supported build and its latest of one major (opencode-1,
	# opencode-2): every scenario runs twice in each.
	opencode) echo 'OpenCode' ;;
	others) echo '.' ;;
	*) echo "unknown shard $1; one of: claude codex-a codex-b copilot opencode-1 opencode-2 others" >&2; exit 2 ;;
	esac
}

shard_of() {
	for s in $SHARDS; do
		if echo "$1" | grep -Eq "$(pattern "$s")"; then
			echo "$s"
			return
		fi
	done
}

[ "$1" = list ] || pattern "$1" >/dev/null
# -list compiles the package without running it; TERMA_E2E=0 keeps TestMain from writing a report.
tests=$(cd "$ROOT/test/e2e" && TERMA_E2E=0 go test -list "$2" . | grep '^Test')

picked=""
for t in $tests; do
	s=$(shard_of "$t")
	if [ "$1" = list ]; then
		printf '%-10s %s\n' "$s" "$t"
	elif [ "$s" = "$1" ]; then
		picked="${picked:+$picked|}$t"
	fi
done
[ "$1" = list ] && exit 0
[ -n "$picked" ] || { echo "shard $1 has no tests" >&2; exit 1; }
echo "^($picked)\$"
