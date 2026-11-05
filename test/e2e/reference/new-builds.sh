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
# check, or the night, catches up.
set -euo pipefail

base=${TERMA_REFERENCE_URL:-https://raw.githubusercontent.com/miradorlabs/terma-cli/compat-matrix/docs/harnesses}
max=${TERMA_REFERENCE_MAX:-5}

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
		releases=$(jq -r '.versions | keys[]' <<<"$doc" | grep -E '^[0-9]+(\.[0-9]+)*$' | sort -V)
	fi
	[ -n "$releases" ] || continue
	published=""
	if index=$(curl -fsSL --max-time 30 "$base/$harness/index.json" 2>/dev/null); then
		published=$(jq -r '.versions[].version' <<<"$index" | sort -V)
	fi
	if [ -z "$published" ]; then
		new=$(tail -n 1 <<<"$releases")
	else
		newest=$(tail -n 1 <<<"$published")
		# Newer than the newest published, and not published: an older build released late
		# (a backport) is the night's to judge.
		new=$(printf '%s\n%s\n' "$releases" "$newest" | sort -V -u | sed -n "/^$(sed 's/[.]/\\./g' <<<"$newest")\$/,\$p" | tail -n +2 |
			grep -vxF -f <(printf '%s\n' "$published") | tail -n "$max" || true)
	fi
	[ -n "$new" ] || continue
	echo "$harness=$(paste -sd, - <<<"$new")"
done

# The desktop apps (TERMA_E2E_<APP>_APP_VERSIONS), by the update feed each one's updater reads:
# Claude Desktop's Squirrel RELEASES.json names its current release alone, the Codex app's
# Sparkle appcast every release. Only the newest is the frequent check's: the app's update feed
# is all there is to fetch an app by, and an older release than the newest is the night's.
desktop=(
	"claude-desktop https://downloads.claude.ai/releases/darwin/universal/RELEASES.json squirrel"
	"codex-desktop https://persistent.oaistatic.com/codex-app-prod/appcast.xml sparkle"
)
for entry in "${desktop[@]}"; do
	read -r harness feed kind <<<"$entry"
	if ! doc=$(curl -fsSL --max-time 30 "$feed"); then
		echo "::warning::could not read $harness's update feed" >&2
		continue
	fi
	if [ "$kind" = squirrel ]; then
		newest=$(jq -r '.releases[].updateTo.version // empty' <<<"$doc" | sort -V | tail -n 1)
	else
		newest=$(grep -o '<sparkle:shortVersionString>[^<]*' <<<"$doc" | sed 's/.*>//' | sort -V | tail -n 1)
	fi
	[ -n "$newest" ] || continue
	if index=$(curl -fsSL --max-time 30 "$base/$harness/index.json" 2>/dev/null) &&
		jq -r '.versions[].version' <<<"$index" | grep -qxF "$newest"; then
		continue
	fi
	echo "$harness=$newest"
done
