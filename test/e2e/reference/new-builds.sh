#!/usr/bin/env bash
# Which harness builds are out that the Harness Reference has not yet published, so the
# frequent check (reference.yml) captures them within the quarter hour instead of waiting
# for the night. A few requests a harness: its registry's releases, and the published
# index. Prints one line a harness with new builds, "harness=version,version", oldest
# first, at most TERMA_REFERENCE_MAX (5) of a burst, as the nightly's `checked` does; a
# harness the reference has never published is its newest build alone. Prints nothing when
# nothing is new.
#
# A registry or the reference that cannot be read is a warning, not a failure: the next
# check, or the night, catches up. Only a reference that says it has no index for a harness
# (404) makes it a harness never published.
set -euo pipefail

base=${TERMA_REFERENCE_URL:-https://raw.githubusercontent.com/miradorlabs/terma-cli/compat-matrix/docs/harnesses}
max=${TERMA_REFERENCE_MAX:-5}

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
)

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
		new=$(tail -n 1 <<<"$releases")
	else
		newest=$(tail -n 1 <<<"$published")
		# Newer than the newest published, and not published: an older build released late
		# (a backport) is the night's to judge.
		new=$(printf '%s\n%s\n' "$releases" "$newest" | vsort -u | sed -n "/^$(sed 's/[.]/\\./g' <<<"$newest")\$/,\$p" | tail -n +2 |
			grep -vxF -f <(printf '%s\n' "$published") | tail -n "$max" || true)
	fi
	[ -n "$new" ] || continue
	echo "$harness=$(paste -sd, - <<<"$new")"
done

# The harnesses published by an update feed rather than a registry, by the feed their updater
# reads: the desktop apps' (TERMA_E2E_<APP>_APP_VERSIONS), Claude Desktop's Squirrel
# RELEASES.json naming its current release alone, the Codex app's Sparkle appcast every release;
# and Muse's channel (TERMA_E2E_MUSE_VERSIONS), its current build alone. Only the newest is the
# frequent check's: the feed is all there is to fetch a build by, and an older one than the
# newest is the night's.
desktop=(
	"claude-desktop https://downloads.claude.ai/releases/darwin/universal/RELEASES.json squirrel"
	"codex-desktop https://persistent.oaistatic.com/codex-app-prod/appcast.xml sparkle"
	"muse https://api.meta.ai/muse-code/channels/muse-stable channel"
)
for entry in "${desktop[@]}"; do
	read -r harness feed kind <<<"$entry"
	if ! doc=$(curl -fsSL --max-time 30 "$feed"); then
		echo "::warning::could not read $harness's update feed" >&2
		continue
	fi
	if [ "$kind" = squirrel ]; then
		newest=$(jq -r '.releases[].updateTo.version // empty' <<<"$doc" | vsort | tail -n 1)
	elif [ "$kind" = channel ]; then
		newest=$(jq -r '.version // empty' <<<"$doc" | grep -E '^[0-9A-Za-z][0-9A-Za-z.+_-]*$' || true)
	else
		newest=$(grep -o '<sparkle:shortVersionString>[^<]*' <<<"$doc" | sed 's/.*>//' | vsort | tail -n 1)
	fi
	[ -n "$newest" ] || continue
	status=0
	read_index "$harness" || status=$?
	[ "$status" -ne 2 ] || continue
	if [ "$status" -eq 0 ] && jq -r '.versions[].version' <<<"$index" | grep -qxF "$newest"; then
		continue
	fi
	echo "$harness=$newest"
done
