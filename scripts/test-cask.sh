#!/bin/bash
# Installs the Homebrew cask GoReleaser rendered into dist/homebrew, from a throwaway
# local tap, with its download URLs pointed at a loopback mirror of dist/. macOS only.
# Runs in CI (homebrew-cask job) after a tagged dry run, and locally via
# `make test-install`.
#
#   scripts/test-cask.sh <dist-dir> <tag>
#
# This is the check that the cask a release pushes to miradorlabs/homebrew-tap will
# install: `brew audit --strict` on the rendered file, then a real `brew install`
# that downloads, verifies the sha256, runs the preflight that strips the quarantine
# flag, generates completions by executing the binary, and links it. `brew style` is
# reported but not enforced — stanza order is GoReleaser's template, not ours.
set -euo pipefail

DIST="${1:?usage: test-cask.sh <dist-dir> <tag>}"
TAG="${2:?usage: test-cask.sh <dist-dir> <tag>}"
PORT="${TERMA_TEST_PORT:-18766}"
TAPNAME="terma-ci/local"

[ "$(uname -s)" = Darwin ] || { echo "test-cask.sh: macOS only (Homebrew casks)" >&2; exit 2; }
command -v brew >/dev/null || { echo "test-cask.sh: brew not found" >&2; exit 2; }
[ -f "$DIST/homebrew/Casks/terma.rb" ] || { echo "test-cask.sh: no cask in $DIST/homebrew/Casks" >&2; exit 1; }

work="$(mktemp -d)"
mirror="$work/mirror/miradorlabs/terma-cli/releases/download/$TAG"
mkdir -p "$mirror"
cp "$DIST"/*.tar.gz "$DIST"/checksums.txt "$mirror/"

(cd "$work/mirror" && exec python3 -m http.server "$PORT" --bind 127.0.0.1 >"$work/http.log" 2>&1) &
server=$!
disown # no "Terminated" notice from the shell when cleanup kills it
cleanup() {
  kill $server 2>/dev/null || true
  brew uninstall --cask terma >/dev/null 2>&1 || true
  brew untap "$TAPNAME" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
for _ in $(seq 1 50); do curl -fs -o /dev/null "http://127.0.0.1:$PORT/" && break; sleep 0.1; done

prefix="$(brew --prefix)"
[ ! -e "$prefix/bin/terma" ] || { echo "test-cask.sh: $prefix/bin/terma already exists; refusing to overwrite" >&2; exit 2; }
fail() { echo "FAIL: $*" >&2; exit 1; }

brew tap-new "$TAPNAME" --no-git >/dev/null
tap="$(brew --repository)/Library/Taps/terma-ci/homebrew-local"
mkdir -p "$tap/Casks"
# Only the host changes; the path, the version interpolation and the sha256 are the
# cask's own.
sed "s#https://github.com#http://127.0.0.1:$PORT#" "$DIST/homebrew/Casks/terma.rb" >"$tap/Casks/terma.rb"
grep -q 'download/v#{version}/' "$tap/Casks/terma.rb" || fail "cask URL is not versioned (was dist rendered from a tag?)"
# GoReleaser's hooks.* render as the block stanzas Homebrew deprecated on 2026-08-04
# ("Calling `preflight` is deprecated! Use `preflight_steps` instead." on every
# install). The quarantine strip is a preflight_steps custom_block instead; keep it so.
if grep -Eq '^\s*(uninstall_)?(pre|post)flight do' "$tap/Casks/terma.rb"; then
  fail "cask uses a deprecated flight block; use preflight_steps via custom_block (see .goreleaser.yaml)"
fi

echo "== brew style (informational)"
brew style --cask "$tap/Casks/terma.rb" || echo "brew style has findings (GoReleaser's template; not enforced)"

echo "== brew audit --strict"
brew audit --cask --strict "$TAPNAME/terma" || fail "brew audit"

echo "== brew install"
brew trust "$TAPNAME" >/dev/null 2>&1 || true # Homebrew >= 6 will not load an untrusted third-party tap
HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1 brew install --cask "$TAPNAME/terma"
# `brew install` also loads every cask already on the machine, so its output cannot
# tell whose deprecation warning it is; ask about this cask alone.
if brew info --cask "$TAPNAME/terma" 2>&1 | grep -q "is deprecated!"; then
  brew info --cask "$TAPNAME/terma" 2>&1 | grep "is deprecated!" >&2
  fail "the cask uses a stanza Homebrew has deprecated"
fi

echo "== installed binary"
want="terma ${TAG#v}"
got="$("$prefix/bin/terma" version)"
[ "$got" = "$want" ] || fail "terma version: got '$got', want '$want'"
staged="$(readlink "$prefix/bin/terma")"
if xattr -p com.apple.quarantine "$staged" >/dev/null 2>&1; then fail "quarantine flag still set on $staged"; fi
for f in etc/bash_completion.d/terma share/zsh/site-functions/_terma share/fish/vendor_completions.d/terma.fish; do
  [ -e "$prefix/$f" ] || fail "missing completion $prefix/$f"
done

echo "ok: cask installs ($(grep -c GET "$work/http.log") requests served)"
