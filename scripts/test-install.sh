#!/bin/bash
# Exercises install.sh against a GoReleaser render in dist/, served over loopback the
# way GitHub Releases would serve it. Runs in CI (install-script job) after a tagged
# dry run, and locally via `make test-install`.
#
#   scripts/test-install.sh <dist-dir> <tag>
#
# Covers the paths a user takes: pinned version piped through bash (the documented
# `curl | bash`), latest under plain sh, a version without the v prefix, and the two
# refusals — a checksum that does not match, and cleartext to a host that is not this
# machine. Nothing here touches the real PATH: every install goes to a temp dir.
set -euo pipefail

DIST="${1:?usage: test-install.sh <dist-dir> <tag>}"
TAG="${2:?usage: test-install.sh <dist-dir> <tag>}"
PORT="${TERMA_TEST_PORT:-18765}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INSTALLER="$ROOT/install.sh"

work="$(mktemp -d)"
mirror="$work/mirror/miradorlabs/terma-cli/releases"
mkdir -p "$mirror/download/$TAG" "$mirror/latest"
cp "$DIST"/*.tar.gz "$DIST"/checksums.txt "$mirror/download/$TAG/"
ln -s "../download/$TAG" "$mirror/latest/download"

(cd "$work/mirror" && exec python3 -m http.server "$PORT" --bind 127.0.0.1 >"$work/http.log" 2>&1) &
server=$!
disown # no "Terminated" notice from the shell when cleanup kills it
trap 'kill $server 2>/dev/null || true; rm -rf "$work"' EXIT
for _ in $(seq 1 50); do curl -fs -o /dev/null "http://127.0.0.1:$PORT/" && break; sleep 0.1; done

BASE="http://127.0.0.1:$PORT/miradorlabs/terma-cli/releases"
want="terma ${TAG#v}"
fail() { echo "FAIL: $*" >&2; exit 1; }

echo "== pinned version, piped through bash"
TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$work/bin1" bash <"$INSTALLER"
[ "$("$work/bin1/terma" version)" = "$want" ] || fail "pinned install: $("$work/bin1/terma" version)"

echo "== latest, under sh"
TERMA_RELEASE_BASE="$BASE" TERMA_INSTALL_DIR="$work/bin2" sh "$INSTALLER" 2>/dev/null
[ "$("$work/bin2/terma" version)" = "$want" ] || fail "latest install: $("$work/bin2/terma" version)"

echo "== version without the v prefix"
TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="${TAG#v}" TERMA_INSTALL_DIR="$work/bin3" sh "$INSTALLER" 2>/dev/null
[ -x "$work/bin3/terma" ] || fail "unprefixed version did not install"

echo "== refuses a checksum that does not match"
sums="$mirror/download/$TAG/checksums.txt"
cp "$sums" "$work/checksums.good"
sed 's/^[0-9a-f]\{4\}/dead/' "$work/checksums.good" >"$sums"
if TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$work/bin4" sh "$INSTALLER" 2>"$work/err4"; then
  fail "tampered checksums.txt was accepted"
fi
grep -q "checksum mismatch" "$work/err4" || fail "wrong error for tampered checksum: $(cat "$work/err4")"
[ ! -e "$work/bin4/terma" ] || fail "binary installed despite checksum mismatch"
cp "$work/checksums.good" "$sums"

echo "== refuses cleartext to a host that is not loopback"
if TERMA_RELEASE_BASE="http://example.invalid/releases" TERMA_INSTALL_DIR="$work/bin5" sh "$INSTALLER" 2>"$work/err5"; then
  fail "cleartext base was accepted"
fi
grep -q "download failed" "$work/err5" || fail "wrong error for cleartext: $(cat "$work/err5")"

echo "ok: install.sh ($(grep -c GET "$work/http.log") requests served)"
