#!/usr/bin/env bash
# Backfill the Harness Reference with a harness's older builds: run by hand, once, not in
# CI. Each build runs on its own, so one that can no longer be fetched or driven is a line
# in the summary, not the end of the run; a build already captured in the output is skipped,
# so a run stopped half way picks up where it stopped. Downloads stay cached
# (TERMA_E2E_VERSIONS_DIR), so a later run with more probes needs none again.
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
i=0
for v in $versions; do
	i=$((i + 1))
	dir="$out/$harness/$v"
	if [ -s "$dir/profiles.json" ]; then
		continue
	fi
	mkdir -p "$dir"
	start=$(date +%s)
	status=pass
	if ! env TERMA_ENV=dev TERMA_E2E=1 TERMA_E2E_BINARY="$bin/terma" TERMA_E2E_REPORT="$dir" "$var=$v" \
		"$bin/e2e.test" -test.v -test.timeout 15m -test.run "^Test${name}(Profile|Probe)" >"$dir/log" 2>&1; then
		status=fail
	fi
	[ -s "$dir/profiles.json" ] || status="$status, no profile"
	printf '%s\t%s\t%s\t%ss\n' "$harness" "$v" "$status" "$(($(date +%s) - start))" | tee -a "$summary"
	echo "  [$i/$total]" >&2
done
