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

# The installer takes the shell this terminal runs to be its parent process, so each
# case starts it from a stand-in named for the shell it plays.
mkdir -p "$work/shells"
for s in zsh bash fish tcsh; do ln -s /bin/bash "$work/shells/$s"; done
# install_in <terminal shell> <login shell> <home> [VAR=value...] installs to the default
# destination from a terminal running <terminal shell> whose PATH does not have it, and
# prints what the installer said.
install_in() {
  # shellcheck disable=SC2016 # expanded by the stand-in
  env HOME="$3" SHELL="$2" PATH="/usr/bin:/bin" TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" \
    "${@:4}" "$work/shells/$1" -c 'sh "$0"; :' "$INSTALLER" 2>&1 </dev/null
}
# install_as <login shell> <home> [VAR=value...]: the same, from a terminal running it.
install_as() { install_in "${1##*/}" "$@"; }
fresh_home() { mktemp -d "$work/home.XXXXXX"; }
# clean <output> fails on any line the installer did not mean to say, such as a shell's
# own error about an unset variable or a file it cannot write.
clean() {
  local stray
  stray="$(grep -vE '^(Downloading |Installed |Added |Note: |Next: run |.+ is (already )?on PATH in )' <<<"$1" || true)"
  [ -z "$stray" ] || fail "stray output: $stray"
}
# reloads <shell> <home> <output> runs the `source …` of the installer's Next: line in
# <shell> (sh as `.`) and prints where `terma` then resolves.
reloads() {
  local src word=source
  # shellcheck disable=SC2016 # the backticks are the installer's text
  src="$(sed -n 's/^Next: run `source \(.*\)`, then `terma setup`\.$/\1/p' <<<"$3")"
  [ -n "$src" ] || fail "no source command in: $3"
  [ "$1" != sh ] || word=.
  env -i HOME="$2" PATH=/usr/bin:/bin "$1" -c "$word $src && command -v terma" 2>/dev/null
}
# next_runs <home> <output> runs the installer's full-path `Next:` command in sh,
# `version` in place of `setup`, and fails unless that reaches the installed terma.
next_runs() {
  local cmd
  # shellcheck disable=SC2016 # the backticks are the installer's text
  cmd="$(sed -n 's/^Next: run `\([^`]*\) setup`\.$/\1/p' <<<"$2")"
  [ -n "$cmd" ] || fail "no full-path Next command in: $2"
  [ "$(HOME="$1" PATH=/usr/bin:/bin sh -c "$cmd version")" = "$want" ] || fail "\`$cmd version\` is not terma: $2"
}
# shellcheck disable=SC2016 # the line the installer writes, for the startup file to expand
line='export PATH="$HOME/.local/bin:$PATH"'

echo "== adds ~/.local/bin to PATH in the zsh startup file, once, and says what to source here"
home="$(fresh_home)"
printf 'alias ll=ls' >"$home/.zshrc" # no trailing newline
out="$(install_as /bin/zsh "$home")"
clean "$out"
[ "$(cat "$home/.zshrc")" = "$(printf 'alias ll=ls\n# added by the terma installer\n%s' "$line")" ] \
  || fail ".zshrc: $(cat "$home/.zshrc")"
grep -qF "Added $home/.local/bin to PATH in ~/.zshrc, for zsh (your login shell): new zsh terminals will find terma." <<<"$out" \
  || fail "no Added line: $out"
grep -qxF "Next: run \`source ~/.zshrc\`, then \`terma setup\`." <<<"$out" || fail "no source step: $out"
[ "$(reloads sh "$home" "$out")" = "$home/.local/bin/terma" ] || fail "sourcing ~/.zshrc does not put terma on PATH"
[ ! -e "$home/.bashrc" ] || fail "wrote .bashrc for a zsh terminal"
out="$(install_as /bin/zsh "$home")"
clean "$out"
[ "$(grep -cxF "$line" "$home/.zshrc")" = 1 ] || fail "a reinstall added the line again: $(cat "$home/.zshrc")"
grep -qxF "$home/.local/bin is already on PATH in new zsh terminals (~/.zshrc)." <<<"$out" || fail "a reinstall said: $out"
grep -qxF "Next: run \`source ~/.zshrc\`, then \`terma setup\`." <<<"$out" || fail "a reinstall gave no source step: $out"

echo "== a terminal running another shell than the login shell gets its own line, and that file to source"
home="$(fresh_home)"
out="$(install_in bash /bin/zsh "$home")"
clean "$out"
for f in .zshrc .bashrc .profile; do grep -qxF "$line" "$home/$f" || fail "$f has no PATH line: $out"; done
if ! grep -qF "in ~/.zshrc, for zsh (your login shell)" <<<"$out" \
  || ! grep -qF "in ~/.bashrc and ~/.profile, for bash (the shell that ran the installer)" <<<"$out"; then
  fail "a bash terminal with zsh to log in, but said: $out"
fi
grep -qxF "Next: run \`source ~/.bashrc\`, then \`terma setup\`." <<<"$out" || fail "no bash source step: $out"
[ "$(reloads /bin/bash "$home" "$out")" = "$home/.local/bin/terma" ] || fail "sourcing ~/.bashrc in bash does not find terma"
home="$(fresh_home)"
out="$(install_in bash /bin/tcsh "$home")" # a login shell it cannot write for
clean "$out"
if ! grep -qF "for bash (the shell that ran the installer)" <<<"$out" \
  || ! grep -qxF "Next: run \`source ~/.bashrc\`, then \`terma setup\`." <<<"$out"; then
  fail "a bash terminal with tcsh to log in, but said: $out"
fi
home="$(fresh_home)"
out="$(install_in tcsh /bin/zsh "$home")" # a shell it has no source step for: the full path
clean "$out"
if ! grep -qxF "$line" "$home/.zshrc" || [ -e "$home/.bashrc" ] || grep -q 'source' <<<"$out"; then
  fail "a tcsh terminal with zsh to log in, but said: $out"
fi
next_runs "$home" "$out"

echo "== zsh with ZDOTDIR, quoted where the path needs it"
home="$(fresh_home)"
out="$(install_as /bin/zsh "$home" ZDOTDIR="$home/z'sh")"
clean "$out"
grep -qF "in '$home/z'\\''sh/.zshrc', for zsh" <<<"$out" || fail "ZDOTDIR shown unquoted: $out"
[ "$(reloads sh "$home" "$out")" = "$home/.local/bin/terma" ] || fail "the quoted source command does not work: $out"

echo "== bash: .bashrc for an interactive shell and the login file for a login shell, both read by a real bash"
home="$(fresh_home)"
out="$(install_as /bin/bash "$home")"
clean "$out"
grep -qF "in ~/.bashrc and ~/.profile, for bash (your login shell)" <<<"$out" || fail "bash said: $out"
[ "$(reloads /bin/bash "$home" "$out")" = "$home/.local/bin/terma" ] || fail "bash's source step does not find terma: $out"
for mode in -ic -lc; do
  [ "$(env -i HOME="$home" PATH=/usr/bin:/bin /bin/bash "$mode" 'command -v terma' 2>/dev/null)" = "$home/.local/bin/terma" ] \
    || fail "bash $mode does not find terma: $(ls -A "$home")"
done
home="$(fresh_home)"
: >"$home/.bash_profile" # the login file bash reads, so no .profile appears
clean "$(install_as /bin/bash "$home")"
if ! grep -qxF "$line" "$home/.bashrc" || ! grep -qxF "$line" "$home/.bash_profile" || [ -e "$home/.profile" ]; then
  fail "with .bash_profile: $(ls -A "$home")"
fi

echo "== bash with a login file it cannot write says which terminals miss out"
if [ "$(id -u)" != 0 ]; then # root writes it anyway
  home="$(fresh_home)"
  : >"$home/.bash_profile"; chmod 444 "$home/.bash_profile"
  out="$(install_as /bin/bash "$home")"
  clean "$out"
  if ! grep -qF "is on PATH in ~/.bashrc, for bash (your login shell), but ~/.bash_profile could not be written" <<<"$out" \
    || grep -q '^Added' <<<"$out" || ! grep -qxF "$line" "$home/.bashrc" || [ -s "$home/.bash_profile" ]; then
    fail "a read-only .bash_profile, but said: $out"
  fi
  grep -qxF "Next: run \`source ~/.bashrc\`, then \`terma setup\`." <<<"$out" || fail "no source step for the file it wrote: $out"
fi

echo "== fish: a conf.d file of terma's own, under XDG_CONFIG_HOME when it is set"
home="$(fresh_home)"
out="$(install_as /usr/bin/fish "$home")"
clean "$out"
grep -qxF "Next: run \`source ~/.config/fish/conf.d/terma.fish\`, then \`terma setup\`." <<<"$out" || fail "no fish source step: $out"
clean "$(install_as /usr/bin/fish "$home" XDG_CONFIG_HOME="$home/xdg")"
for conf in "$home/.config" "$home/xdg"; do
  # shellcheck disable=SC2016 # fish expands it
  grep -qxF 'fish_add_path --move --prepend "$HOME/.local/bin"' "$conf/fish/conf.d/terma.fish" \
    || fail "terma.fish: $(cat "$conf/fish/conf.d/terma.fish" 2>&1)"
done
if fish="$(command -v fish)"; then # CI installs it on Linux
  [ "$(env -i HOME="$home" PATH=/usr/bin:/bin "$fish" -c 'command -v terma')" = "$home/.local/bin/terma" ] \
    || fail "fish does not find terma through terma.fish"
  [ "$(reloads "$fish" "$home" "$out")" = "$home/.local/bin/terma" ] || fail "fish's source step does not find terma"
fi

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
note "$(install_as /bin/zsh "$home" TERMA_NO_MODIFY_PATH=)" "opted out, empty"
note "$(install_as /bin/tcsh "$home")" "unknown shell"
if [ -x /bin/dash ]; then # Debian's sh; bash fills SHELL in from the user database
  # shellcheck disable=SC2016 # expanded by the outer dash, which plays no known shell
  note "$(env -u SHELL HOME="$home" PATH=/usr/bin:/bin TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" \
    /bin/dash -c '/bin/dash "$0"; :' "$INSTALLER" 2>&1 </dev/null)" "SHELL unset under dash"
fi
[ "$(ls -A "$home")" = .local ] || fail "wrote $(ls -A "$home")"

echo "== a startup file it cannot write or read gets the note, not the shell's error"
if [ "$(id -u)" != 0 ]; then # root writes it anyway
  home="$(fresh_home)"
  : >"$home/.zshrc"; chmod 444 "$home/.zshrc"
  note "$(install_as /bin/zsh "$home")" "unwritable .zshrc"
  [ ! -s "$home/.zshrc" ] || fail "wrote the read-only .zshrc"
  home="$(fresh_home)"
  printf 'x' >"$home/.zshrc"; chmod 200 "$home/.zshrc"
  note "$(install_as /bin/zsh "$home")" "unreadable .zshrc"
  chmod 600 "$home/.zshrc"
  [ "$(cat "$home/.zshrc")" = x ] || fail "wrote the unreadable .zshrc: $(cat "$home/.zshrc")"
fi

echo "== a directory outside home is written literally, every metacharacter inert"
home="$(fresh_home)"
odd="$work/a b\"c\$d\`e\\f'g"
out="$(install_as /bin/zsh "$home" TERMA_INSTALL_DIR="$odd")"
clean "$out"
[ "$(reloads sh "$home" "$out")" = "$odd/terma" ] || fail "sourcing ~/.zshrc does not find $odd/terma: $(cat "$home/.zshrc")"

echo "== a directory with a colon, which PATH would split, is never written"
home="$(fresh_home)"
out="$(install_as /bin/zsh "$home" TERMA_INSTALL_DIR="$work/a:b")"
clean "$out"
if ! grep -qF "cannot go on PATH" <<<"$out" || ! grep -qF "Next: run \`'$work/a:b/terma' setup\`." <<<"$out"; then
  fail "colon in the directory, but said: $out"
fi
next_runs "$home" "$out"
[ ! -e "$home/.zshrc" ] || fail "wrote .zshrc for a directory PATH cannot hold: $(cat "$home/.zshrc")"

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
