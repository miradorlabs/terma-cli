#!/usr/bin/env bash
# Backfill the Harness Reference with a harness's older builds: run by hand, once, not in
# CI. Each build runs on its own, so one that can no longer be fetched or driven is a line
# in the summary, not the end of the run; a build already captured in the output is skipped,
# so a run stopped half way picks up where it stopped; BACKFILL_JOBS builds run at once. Each build is downloaded into the
# run's own folder and removed once captured: a hundred builds never sit on disk at once.
# (Its own folder, too, because macOS holds at launch whatever is put at a path it once
# rejected, and an older run may have had a revoked build rejected at the shared cache's.)
#
#   test/e2e/reference/backfill.sh claude 100 ../../live/backfill
#   test/e2e/reference/backfill.sh codex 100 ../../live/backfill
#
# then render the reference from every profile captured (refgen's -profiles, repeated).
set -euo pipefail

harness=${1:?usage: backfill.sh <claude|codex|gemini|opencode|copilot> <count> [outdir]}
count=${2:?usage: backfill.sh <harness> <count> [outdir]}
cd "$(dirname "$0")/.."
out=$(mkdir -p "${3:-backfill}" && cd "${3:-backfill}" && pwd)

case "$harness" in
claude) pkg=@anthropic-ai/claude-code name=Claude ;;
codex) pkg=@openai/codex name=Codex ;;
gemini) pkg=@google/gemini-cli name=Gemini ;;
opencode) pkg=opencode-ai name=OpenCode ;;
copilot) pkg=@github/copilot name=Copilot ;;
*)
	echo "backfill.sh: no backfill for $harness" >&2
	exit 2
	;;
esac
var="TERMA_E2E_$(tr '[:lower:]' '[:upper:]' <<<"$harness")_VERSIONS"

versions=$(curl -fsSL -H "Accept: application/vnd.npm.install-v1+json" "https://registry.npmjs.org/${pkg//\//%2F}" |
	jq -r '.versions | keys[]' | grep -E '^[0-9]+(\.[0-9]+)*$' | sort -V | tail -n "$count")

# One terma and one test binary for the whole run; a parallel run of another harness
# builds its own.
bin="$out/.bin-$harness"
mkdir -p "$bin"
(cd ../.. && go build -o "$bin/terma" ./cmd/terma)
go test -c -o "$bin/e2e.test" .

summary="$out/summary.tsv"
total=$(wc -l <<<"$versions" | tr -d ' ')

# One build: its profile, its proxied first turn and its behaviour probes, not the telemetry
# export's cadence (which holds a model call for a minute, and is better measured on the builds
# the nights test).
one() {
	local v=$1 dir="$out/$harness/$1" start status=pass
	if [ -s "$dir/profiles.json" ]; then
		return
	fi
	mkdir -p "$dir"
	start=$(date +%s)
	if ! env TERMA_ENV=dev TERMA_E2E=1 TERMA_E2E_BINARY="$bin/terma" TERMA_E2E_REPORT="$dir" "$var=$v" \
		TERMA_E2E_VERSIONS_DIR="$out/.versions" \
		"$bin/e2e.test" -test.v -test.timeout 30m -test.run "^Test${name}(Profile|Probe)" -test.skip 'ProfileTelemetry|Offline$' \
		>"$dir/log" 2>&1; then
		status=fail
	fi
	[ -s "$dir/profiles.json" ] || status="$status, no profile"
	rm -rf "$out/.versions/$harness/$v"
	printf '%s\t%s\t%s\t%ss\n' "$harness" "$v" "$status" "$(($(date +%s) - start))" | tee -a "$summary"
}

# BACKFILL_JOBS builds at once (default 3): each in its own sandboxes and download folder.
export harness out bin var name summary
export -f one
echo "$versions" | xargs -P "${BACKFILL_JOBS:-3}" -I{} bash -c 'one "$1"' _ {}
# Once more for a build that failed (a binary macOS removed mid-run, a machine too busy): its
# second result is the one kept.
for v in $(awk -F'\t' -v h="$harness" '$1 == h && $3 ~ /fail/ {print $2}' "$summary" | sort -u); do
	[ -n "$(grep -E "^(--- FAIL)" "$out/$harness/$v/log" 2>/dev/null | grep -v ProbeSubagent)" ] || continue
	rm -rf "${out:?}/$harness/$v"
	echo "$v"
done | xargs -P "${BACKFILL_JOBS:-3}" -I{} bash -c 'one "$1"' _ {}
echo "done: $total builds of $harness" >&2
