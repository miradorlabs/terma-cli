#!/usr/bin/env bash
# Which harness builds the Harness Reference has yet to capture whole, so the frequent check
# (reference.yml) captures each of them once, fully, within the quarter hour: its page and
# changelog are published once, complete, and the night does not capture it again. A few
# requests a harness: its registry's releases, and the published index. Prints one line a
# harness with builds to capture, "harness=version,version", oldest first; prints nothing when
# there are none.
#
# A build to capture is one of a harness's newest TERMA_REFERENCE_MAX (5) releases the reference
# has not published (a backport released late among them), or its newest published build where
# that one's capture was partial and is TERMA_REFERENCE_RETRY_AFTER seconds old (6 h): a capture
# that failed is tried again, but not every quarter hour. A harness the reference has never
# published is its newest build alone.
#
# `new-builds.sh --tracked` prints the harnesses it watches, one a line, and reads nothing: the
# night skips their reference scenarios, which this check's capture takes.
#
# A registry or the reference that cannot be read is a warning, not a failure: the next check
# catches up. Only a reference that says it has no index for a harness (404) makes it a harness
# never published.
set -euo pipefail

base=${TERMA_REFERENCE_URL:-https://raw.githubusercontent.com/miradorlabs/terma-cli/compat-matrix/docs/harnesses}
max=${TERMA_REFERENCE_MAX:-5}
retry_after=${TERMA_REFERENCE_RETRY_AFTER:-21600}

# The builds the suite can fetch by version (TERMA_E2E_<HARNESS>_VERSIONS), each with its
# npm package, read in npm's abbreviated form (versions and tags, not every manifest).
# DeepSeek Harness ships prereleases alone: its newest is npm's latest tag.
harnesses=(
	"claude @anthropic-ai/claude-code releases"
	"codex @openai/codex releases"
	"gemini @google/gemini-cli releases"
	"opencode opencode-ai releases"
	"copilot @github/copilot releases"
	"dsh @deepseek-ai/dsh latest"
	"pi @earendil-works/pi-coding-agent releases"
	"t3 t3 releases"
)

# The desktop apps (TERMA_E2E_<APP>_APP_VERSIONS), by the update feed each one's updater reads:
# Claude Desktop's Squirrel RELEASES.json names its current release alone, the Codex app's
# Sparkle appcast every release. Only the newest is captured: the app's update feed is all
# there is to fetch an app by.
desktop=(
	"claude-desktop https://downloads.claude.ai/releases/darwin/universal/RELEASES.json squirrel"
	"codex-desktop https://persistent.oaistatic.com/codex-app-prod/appcast.xml sparkle"
)

if [ "${1:-}" = --tracked ]; then
	for entry in "${harnesses[@]}" "${desktop[@]}"; do
		echo "${entry%% *}"
	done
	exit 0
fi

# vsort sorts versions as the reference ranks them: it is refdiff.VersionCompare itself
# (refdiff/cmd/versionsort, built once here from test/e2e's go.mod and refdiff alone, no module
# to download), so the frequent check and the published order never disagree. Its arguments are
# versionsort's (-u).
e2e_dir=$(cd "$(dirname "$0")/.." && pwd)
vsort_dir=$(mktemp -d)
trap 'rm -rf "$vsort_dir"' EXIT
(cd "$e2e_dir" && go build -o "$vsort_dir/versionsort" ./refdiff/cmd/versionsort)
vsort() { "$vsort_dir/versionsort" "$@"; }

# read_index sets index to harness's published index.json: status 0 when there is one, 1 when the
# reference has none (404, a harness never published), 2 when it could not be read (a timeout, a
# 5xx), which the caller skips rather than take for never published: an outage of the host
# would otherwise start every capture each quarter hour.
read_index() {
	local code
	index=""
	code=$(curl -sSL --max-time 30 -o "$vsort_dir/index.json" -w '%{http_code}' "$base/$1/index.json" 2>/dev/null) || code=000
	case "$code" in
	200) index=$(cat "$vsort_dir/index.json") ;;
	404) return 1 ;;
	*)
		echo "::warning::could not read $1's published index (HTTP $code)" >&2
		return 2
		;;
	esac
}

# retry is harness's newest published build when its capture was partial and is old enough to
# try again, else nothing.
retry() {
	jq -r --argjson after "$retry_after" --arg newest "$1" '
		[.versions[] | select(.version == $newest)][0] // empty
		| select(.partial == true)
		| select(now - (.captured_at | sub("\\.[0-9]+"; "") | fromdateiso8601) > $after)
		| .version' <<<"$index"
}

for entry in "${harnesses[@]}"; do
	read -r harness pkg mode <<<"$entry"
	if ! doc=$(curl -fsSL --max-time 30 -H "Accept: application/vnd.npm.install-v1+json" "https://registry.npmjs.org/${pkg//\//%2F}"); then
		echo "::warning::could not read $pkg's releases" >&2
		continue
	fi
	if [ "$mode" = latest ]; then
		releases=$(jq -r '.["dist-tags"].latest // empty' <<<"$doc")
	else
		# A release is numbers and dots: no prerelease, as the suite's isRelease says.
		releases=$(jq -r '.versions | keys[]' <<<"$doc" | grep -E '^[0-9]+(\.[0-9]+)*$' | vsort)
	fi
	[ -n "$releases" ] || continue
	published=""
	status=0
	read_index "$harness" || status=$?
	case $status in
	0) published=$(jq -r '.versions[].version' <<<"$index" | vsort) ;;
	2) continue ;;
	esac
	if [ -z "$published" ]; then
		todo=$(tail -n 1 <<<"$releases")
	else
		# The newest releases not published, a backport among them; and the newest published
		# one again, where its capture was partial.
		todo=$({
			tail -n "$max" <<<"$releases" | grep -vxF -f <(printf '%s\n' "$published") || true
			retry "$(tail -n 1 <<<"$published")"
		} | vsort -u)
	fi
	[ -n "$todo" ] || continue
	echo "$harness=$(paste -sd, - <<<"$todo")"
done

for entry in "${desktop[@]}"; do
	read -r harness feed kind <<<"$entry"
	if ! doc=$(curl -fsSL --max-time 30 "$feed"); then
		echo "::warning::could not read $harness's update feed" >&2
		continue
	fi
	if [ "$kind" = squirrel ]; then
		newest=$(jq -r '.releases[].updateTo.version // empty' <<<"$doc" | vsort | tail -n 1)
	else
		newest=$(grep -o '<sparkle:shortVersionString>[^<]*' <<<"$doc" | sed 's/.*>//' | vsort | tail -n 1)
	fi
	[ -n "$newest" ] || continue
	status=0
	read_index "$harness" || status=$?
	[ "$status" -ne 2 ] || continue
	if [ "$status" -eq 0 ] && jq -r '.versions[].version' <<<"$index" | grep -qxF "$newest" && [ -z "$(retry "$newest")" ]; then
		continue
	fi
	echo "$harness=$newest"
done
