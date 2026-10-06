#!/bin/sh
# Renders internal/semconv from the Weaver registry in semconv/registry (`generate`), or
# checks the registry against Weaver's rules and terma's policies, that each policy still
# refuses its fixture, and that the committed code is what the registry renders (`check`).
# Weaver is a development tool: `check` skips when it is not on PATH, and CI installs it.
set -eu
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WEAVER="${WEAVER:-weaver}"
OUT="$ROOT/internal/semconv/semconv.go"

if ! command -v "$WEAVER" >/dev/null 2>&1; then
	[ "${1:-}" = check ] && { echo "weaver not installed; skipping the semconv registry check"; exit 0; }
	echo "semconv: install weaver (https://github.com/open-telemetry/weaver)" >&2
	exit 1
fi

# The registry's dependencies are cloned from GitHub. Offline, a local check skips; CI never does.
if [ "${1:-}" = check ] && [ -z "${CI:-}" ] &&
	! git ls-remote --exit-code -q https://github.com/open-telemetry/semantic-conventions.git v1.44.0 >/dev/null 2>&1; then
	echo "github.com unreachable; skipping the semconv registry check (CI runs it)"
	exit 0
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$ROOT"

render() {
	"$WEAVER" registry generate --quiet --v2 -r semconv/registry -t semconv/templates go "$tmp/gen" >"$tmp/generate.log" 2>&1 ||
		{ cat "$tmp/generate.log" >&2; exit 1; }
	gofmt "$tmp/gen/semconv.go" >"$tmp/semconv.go"
}

case "${1:-}" in
generate)
	render
	mkdir -p "$(dirname "$OUT")"
	cp "$tmp/semconv.go" "$OUT"
	;;
check)
	"$WEAVER" registry check --quiet --v2 -r semconv/registry -p semconv/policies >"$tmp/check.log" 2>&1 ||
		{ cat "$tmp/check.log" >&2; exit 1; }
	# Each fixture breaks one rule; the policy must name it.
	for fixture in semconv/policies/testdata/*/; do
		rule="$(basename "$fixture")"
		if "$WEAVER" registry check --v2 -r "$fixture" -p semconv/policies \
			--diagnostic-format json --diagnostic-stdout >"$tmp/$rule.json" 2>/dev/null; then
			echo "semconv: policy $rule passed its fixture $fixture" >&2
			exit 1
		fi
		grep -q "\"$rule\"" "$tmp/$rule.json" || { echo "semconv: fixture $fixture failed without $rule" >&2; exit 1; }
	done
	render
	cmp -s "$tmp/semconv.go" "$OUT" || { echo "semconv: $OUT is stale; run make semconv" >&2; exit 1; }
	echo "semconv registry ok"
	;;
*)
	echo "usage: $0 generate|check" >&2
	exit 2
	;;
esac
