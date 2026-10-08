#!/bin/bash
# Exercises install.sh against a GoReleaser render in dist/, served over loopback the
# way GitHub Releases would serve it. Runs in CI (install-script job) after a tagged
# dry run, and locally via `make test-install`.
#
#   scripts/test-install.sh <dist-dir> <tag>
#
# Covers the paths a user takes: pinned version piped through bash (the documented
# `curl | bash`), latest under plain sh, a version without the v prefix, the default
# destination (~/.local/bin, never sudo), the PATH line appended for zsh, bash and fish
# (and when it is not), and the two
# refusals — a checksum that does not match, and cleartext to a host that is not this
# machine. Nothing here touches the real PATH: every install goes to a temp dir, under
# a temp HOME.
set -euo pipefail

DIST="${1:?usage: test-install.sh <dist-dir> <tag>}"
TAG="${2:?usage: test-install.sh <dist-dir> <tag>}"
PORT="${TERMA_TEST_PORT:-18765}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INSTALLER="$ROOT/install.sh"

work="$(mktemp -d)"
# `make test-install` exports TERMA_ENV=dev. Keep the release binary's version
# check in that environment too, with no machine profile influencing its output.
export TERMA_ENV=dev TERMA_CONFIG_DIR="$work/config"
# The installer appends to the login shell's startup file; never the developer's own,
# which ZDOTDIR or XDG_CONFIG_HOME would point at whatever HOME is.
export HOME="$work/home"
unset ZDOTDIR XDG_CONFIG_HOME TERMA_INSTALL_DIR TERMA_NO_MODIFY_PATH
mkdir -p "$HOME"
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
want="terma ${TAG#v} (dev)"
fail() { echo "FAIL: $*" >&2; exit 1; }

echo "== pinned version, piped through bash"
TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$work/bin1" bash <"$INSTALLER"
[ "$("$work/bin1/terma" version)" = "$want" ] || fail "pinned install: $("$work/bin1/terma" version)"

echo "== latest, under sh"
TERMA_RELEASE_BASE="$BASE" TERMA_INSTALL_DIR="$work/bin2" sh "$INSTALLER" 2>/dev/null
[ "$("$work/bin2/terma" version)" = "$want" ] || fail "latest install: $("$work/bin2/terma" version)"

echo "== a reinstall renames the new binary into place, leaving nothing staged"
TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$work/bin1" sh "$INSTALLER" 2>/dev/null
[ "$("$work/bin1/terma" version)" = "$want" ] || fail "reinstall: $("$work/bin1/terma" version)"
[ "$(ls -A "$work/bin1")" = terma ] || fail "reinstall left $(ls -A "$work/bin1")"

echo "== version without the v prefix"
TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="${TAG#v}" TERMA_INSTALL_DIR="$work/bin3" sh "$INSTALLER" 2>/dev/null
[ -x "$work/bin3/terma" ] || fail "unprefixed version did not install"

echo "== default: ~/.local/bin, never sudo"
home="$work/home"; mkdir -p "$home"
out="$(HOME="$home" PATH="/usr/bin:/bin" TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" sh "$INSTALLER" 2>&1 </dev/null)"
[ "$("$home/.local/bin/terma" version)" = "$want" ] || fail "default install did not land in ~/.local/bin: $out"
! grep -qi sudo <<<"$out" || fail "installer mentioned sudo: $out"

# install_as <login shell> <home> [VAR=value...] installs to the default destination
# from a shell whose PATH does not have it, and prints what the installer said.
install_as() {
  env HOME="$2" SHELL="$1" PATH="/usr/bin:/bin" TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" \
    "${@:3}" sh "$INSTALLER" 2>&1 </dev/null
}
fresh_home() { mktemp -d "$work/home.XXXXXX"; }
# clean <output> fails on any line the installer did not mean to say, such as a shell's
# own error about an unset variable or a file it cannot write.
clean() {
  local stray
  stray="$(grep -vE '^(Downloading |Installed |Added |Note: |Next: run |.+ already puts )' <<<"$1" || true)"
  [ -z "$stray" ] || fail "stray output: $stray"
}
# finds <home> <output> runs the `source …` the installer suggested, in sh, and prints
# where `terma` then resolves.
finds() {
  local cmd
  # shellcheck disable=SC2016 # the backticks are the installer's text
  cmd="$(sed -n 's/^Next: run `source \(.*\)` in this terminal.*/\1/p' <<<"$2")"
  [ -n "$cmd" ] || fail "no source command in: $2"
  HOME="$1" PATH=/usr/bin:/bin sh -c ". $cmd && command -v terma"
}
# shellcheck disable=SC2016 # the line the installer writes, for the startup file to expand
line='export PATH="$HOME/.local/bin:$PATH"'

echo "== adds ~/.local/bin to PATH in the zsh startup file, once"
home="$(fresh_home)"
printf 'alias ll=ls' >"$home/.zshrc" # no trailing newline
out="$(install_as /bin/zsh "$home")"
clean "$out"
[ "$(cat "$home/.zshrc")" = "$(printf 'alias ll=ls\n# added by the terma installer\n%s' "$line")" ] \
  || fail ".zshrc: $(cat "$home/.zshrc")"
grep -qF "Added $home/.local/bin to your PATH in ~/.zshrc" <<<"$out" || fail "no Added line: $out"
grep -qF "run \`source ~/.zshrc\` in this terminal, then \`terma setup\`" <<<"$out" || fail "no reload hint: $out"
[ "$(finds "$home" "$out")" = "$home/.local/bin/terma" ] || fail "sourcing ~/.zshrc does not put terma on PATH"
out="$(install_as /bin/zsh "$home")"
clean "$out"
[ "$(grep -cxF "$line" "$home/.zshrc")" = 1 ] || fail "a reinstall added the line again: $(cat "$home/.zshrc")"
grep -qF ".zshrc already puts $home/.local/bin on PATH" <<<"$out" || fail "a reinstall said: $out"

echo "== zsh with ZDOTDIR, quoted where the path needs it"
home="$(fresh_home)"
out="$(install_as /bin/zsh "$home" ZDOTDIR="$home/z'sh")"
clean "$out"
grep -qxF "$line" "$home/z'sh/.zshrc" || fail "\$ZDOTDIR/.zshrc has no PATH line: $(ls -AR "$home")"
[ "$(finds "$home" "$out")" = "$home/.local/bin/terma" ] || fail "the suggested source command does not work: $out"

echo "== bash: ~/.bashrc, or on macOS the login file it reads"
home="$(fresh_home)"
rcfile=.bashrc; [ "$(uname -s)" != Darwin ] || rcfile=.bash_profile
install_as /bin/bash "$home" >/dev/null
grep -qxF "$line" "$home/$rcfile" || fail "$rcfile has no PATH line: $(ls -A "$home")"

echo "== fish: a conf.d file of terma's own, under XDG_CONFIG_HOME when it is set"
home="$(fresh_home)"
install_as /usr/bin/fish "$home" >/dev/null
install_as /usr/bin/fish "$home" XDG_CONFIG_HOME="$home/xdg" >/dev/null
for conf in "$home/.config" "$home/xdg"; do
  # shellcheck disable=SC2016 # fish expands it
  grep -qxF 'fish_add_path --move --prepend "$HOME/.local/bin"' "$conf/fish/conf.d/terma.fish" \
    || fail "terma.fish: $(cat "$conf/fish/conf.d/terma.fish" 2>&1)"
done

echo "== nothing written when it is already on PATH, opted out, or the shell is unknown or unset"
home="$(fresh_home)"
out="$(install_as /bin/zsh "$home" PATH="$home/.local/bin:/usr/bin:/bin")"
clean "$out"
if [ "$(tail -n 1 <<<"$out")" != "Next: run \`terma setup\`." ] || grep -q '^Added' <<<"$out"; then
  fail "already on PATH, but said: $out"
fi
note() {
  clean "$1"
  if ! grep -qF "is not on your PATH" <<<"$1" || ! grep -qF "Next: run \`~/.local/bin/terma setup\`." <<<"$1"; then
    fail "$2, but said: $1"
  fi
}
note "$(install_as /bin/zsh "$home" TERMA_NO_MODIFY_PATH=1)" "opted out"
note "$(install_as /bin/tcsh "$home")" "unknown shell"
if [ -x /bin/dash ]; then # Debian's sh; bash fills SHELL in from the user database
  note "$(env -u SHELL HOME="$home" PATH=/usr/bin:/bin TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" \
    /bin/dash "$INSTALLER" 2>&1 </dev/null)" "SHELL unset under dash"
fi
[ "$(ls -A "$home")" = .local ] || fail "wrote $(ls -A "$home")"

echo "== a startup file it cannot write gets the note, not the shell's error"
if [ "$(id -u)" != 0 ]; then # root writes it anyway
  home="$(fresh_home)"
  : >"$home/.zshrc"; chmod 444 "$home/.zshrc"
  note "$(install_as /bin/zsh "$home")" "unwritable .zshrc"
  [ ! -s "$home/.zshrc" ] || fail "wrote the read-only .zshrc"
fi

echo "== a directory outside home is written literally, every metacharacter inert"
home="$(fresh_home)"
odd="$work/a b\"c\$d\`e\\f'g"
out="$(install_as /bin/zsh "$home" TERMA_INSTALL_DIR="$odd")"
clean "$out"
[ "$(finds "$home" "$out")" = "$odd/terma" ] || fail "sourcing ~/.zshrc does not find $odd/terma: $(cat "$home/.zshrc")"

echo "== a relative TERMA_INSTALL_DIR goes on PATH absolute, without .."
home="$(fresh_home)"
mkdir -p "$work/sub"
(cd "$work/sub" && install_as /bin/zsh "$home" TERMA_INSTALL_DIR=../rel/bin >/dev/null)
grep -qxF "export PATH=\"$work/rel/bin:\$PATH\"" "$home/.zshrc" || fail ".zshrc: $(cat "$home/.zshrc")"

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
