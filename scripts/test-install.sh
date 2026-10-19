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
# (and when it is not), and the refusals — a release its key did not sign or signed for
# another tag, a checksums.txt changed after it was signed, no signature, a checksum that
# does not match, and cleartext to a host that is not this machine. Nothing here touches
# the real PATH: every install goes to a temp dir, under a temp HOME.
#
# The mirror is signed with a key made for this run (scripts/releasesign, so Go is
# needed) and installed with a copy of install.sh that pins that key in place of the
# release key; install.sh itself, with the release key, is shown to refuse it.
set -euo pipefail

DIST="${1:?usage: test-install.sh <dist-dir> <tag>}"
TAG="${2:?usage: test-install.sh <dist-dir> <tag>}"
PORT="${TERMA_TEST_PORT:-18765}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RELEASED="$ROOT/install.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
# Built before HOME moves, so Go keeps its own caches and toolchain.
(cd "$ROOT" && go build -o "$work/releasesign" ./scripts/releasesign)
# `make test-install` exports TERMA_ENV=dev. Keep the release binary's version
# check in that environment too, with no machine profile influencing its output.
export TERMA_ENV=dev TERMA_CONFIG_DIR="$work/config"
# The installer appends to the login shell's startup file; never the developer's own,
# which ZDOTDIR or XDG_CONFIG_HOME would point at whatever HOME is.
export HOME="$work/home"
unset ZDOTDIR XDG_CONFIG_HOME TERMA_INSTALL_DIR TERMA_NO_MODIFY_PATH
mkdir -p "$HOME"
mirror="$work/mirror/miradorlabs/terma-cli/releases"
mkdir -p "$mirror/download/$TAG"
cp "$DIST"/*.tar.gz "$DIST"/checksums.txt "$mirror/download/$TAG/"
sums="$mirror/download/$TAG/checksums.txt"

# python3's file server, plus GitHub's redirect from releases/latest to the latest tag,
# and a file beside a <file>.endless sent over and over with no length, until the client
# hangs up (or 64 MiB), logging how much it sent.
cat >"$work/serve.py" <<'EOF'
import functools, http.server, os, sys

root, port, tag = sys.argv[1], int(sys.argv[2]), sys.argv[3]

class Mirror(http.server.SimpleHTTPRequestHandler):
    def send_head(self):
        path = self.path.split('?', 1)[0].rstrip('/')
        if path.endswith('/releases/latest'):
            self.send_response(302)
            self.send_header('Location', path[:-len('latest')] + 'tag/' + tag)
            self.send_header('Content-Length', '0')
            self.end_headers()
            return None
        file = self.translate_path(self.path)
        if os.path.exists(file + '.endless'):
            self.send_response(200)
            self.end_headers()
            with open(file, 'rb') as f:
                data = f.read() or b'\n'
            sent = 0
            try:
                while sent < 64 << 20:
                    self.wfile.write(data)
                    sent += len(data)
            except OSError:
                pass
            self.log_message('streamed %d bytes of %s', sent, self.path)
            return None
        return super().send_head()

http.server.ThreadingHTTPServer(('127.0.0.1', port), functools.partial(Mirror, directory=root)).serve_forever()
EOF
python3 -I "$work/serve.py" "$work/mirror" "$PORT" "$TAG" >"$work/http.log" 2>&1 &
server=$!
disown # no "Terminated" notice from the shell when cleanup kills it
trap 'kill $server 2>/dev/null || true; rm -rf "$work"' EXIT
for _ in $(seq 1 50); do curl -fs -o /dev/null "http://127.0.0.1:$PORT/" && break; sleep 0.1; done

BASE="http://127.0.0.1:$PORT/miradorlabs/terma-cli/releases"
want="terma ${TAG#v} (dev)"
fail() { echo "FAIL: $*" >&2; exit 1; }

# This run's release key, and the installer that pins it in place of the release key.
pub="$("$work/releasesign" keygen "$work/release.key" | sed -n 's/^public key: //p')"
# sign <tag> signs the mirror's checksums.txt for tag, as a release is signed.
sign() { TERMA_SIGNING_KEY="$(cat "$work/release.key")" "$work/releasesign" sign "$1" "$sums" "$sums.sig"; }
sign "$TAG"
# spki_of <hex key> prints it as RELEASE_KEYS holds one: its DER SubjectPublicKeyInfo, base64.
spki_of() { python3 -I -c 'import base64, sys; print(base64.b64encode(bytes.fromhex("302a300506032b6570032100" + sys.argv[1])).decode())' "$1"; }
spki="$(spki_of "$pub")"
INSTALLER="$work/install.sh"
sed "s|^RELEASE_KEYS=.*|RELEASE_KEYS='$spki'|" "$RELEASED" >"$INSTALLER"
[ "$(diff "$RELEASED" "$INSTALLER" | grep -c '^>')" = 1 ] || fail "pinning this run's key changed more than RELEASE_KEYS"

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
  stray="$(grep -vE '^(Downloading |Installed |Added |Note: |Next: run |.+ is (already )?on PATH in |Warning: .+ signature is not checked: that needs OpenSSL 3, )' <<<"$1" || true)"
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

# refused <installer> <message> [VAR=value...]: installs the pinned release with that
# installer to a directory of its own, and fails unless it refuses, saying message, and
# installs nothing.
refused() {
  local dest out
  dest="$(mktemp -d "$work/refused.XXXXXX")"
  if out="$(env TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$dest" "${@:3}" sh "$1" 2>&1)"; then
    fail "installed, where it should have said \"$2\": $out"
  fi
  grep -qF "$2" <<<"$out" || fail "refused, but without \"$2\": $out"
  [ ! -e "$dest/terma" ] || fail "installed terma while refusing: $out"
}
cp "$sums" "$work/checksums.good"
cp "$sums.sig" "$work/checksums.sig.good"
restore() { cp "$work/checksums.good" "$sums"; cp "$work/checksums.sig.good" "$sums.sig"; }
unsigned="is not signed by a key this installer trusts"

# The signature is checked where an OpenSSL 3 is on PATH: the Linux runner's own, or on
# a Mac Homebrew's, linked or not.
ssl=''
for o in "$(command -v openssl || true)" /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl@3/bin/openssl; do
  case "$([ -x "$o" ] && "$o" version)" in "OpenSSL "[3-9]*) ssl="$(dirname "$o")"; break ;; esac
done
if [ -z "$ssl" ]; then
  [ -z "${CI:-}" ] || fail "no OpenSSL 3 here to check signatures with"
  echo "== no OpenSSL 3 here: skipping the signature checks"
else
  echo "== with OpenSSL 3, the signature is checked, and nothing said when it holds"
  out="$(PATH="$ssl:$PATH" TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$work/bin6" sh "$INSTALLER" 2>&1)"
  [ "$("$work/bin6/terma" version)" = "$want" ] || fail "signed install: $out"
  ! grep -q 'signature' <<<"$out" || fail "a signed install said: $out"

  echo "== install.sh, with the release key, refuses a release that key did not sign"
  refused "$RELEASED" "$unsigned" PATH="$ssl:$PATH"

  echo "== refuses a signature made for another tag"
  sign v9.9.9
  refused "$INSTALLER" "$unsigned" PATH="$ssl:$PATH"
  restore

  echo "== refuses a checksums.txt changed after it was signed"
  sed 's/^[0-9a-f]\{4\}/dead/' "$work/checksums.good" >"$sums"
  refused "$INSTALLER" "$unsigned" PATH="$ssl:$PATH"
  restore

  echo "== reads checksums.txt.sig as terma update does: any line, by any pinned key"
  other="$("$work/releasesign" keygen "$work/other.key" | sed -n 's/^public key: //p')"
  "$work/releasesign" keygen "$work/unpinned.key" >/dev/null
  # line <key file> prints the line that key signs the mirror's checksums.txt with.
  line() { TERMA_SIGNING_KEY="$(cat "$1")" "$work/releasesign" sign "$TAG" "$sums" "$work/line.sig" && cat "$work/line.sig"; }
  # accepts <installer> <case>: installs the pinned release with that installer, and fails
  # unless it does.
  accepts() {
    local dest out
    dest="$(mktemp -d "$work/accepted.XXXXXX")"
    out="$(PATH="$ssl:$PATH" TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$dest" sh "$1" 2>&1)" \
      || fail "$2: $out"
    [ "$("$dest/terma" version)" = "$want" ] || fail "$2: $out"
  }
  rotating="$work/install-rotating.sh" # pins two keys, as a release that rotates one does
  sed "s|^RELEASE_KEYS=.*|RELEASE_KEYS='$(spki_of "$other") $spki'|" "$RELEASED" >"$rotating"
  printf 'not a signature\n%s\n%s\n' "$(line "$work/unpinned.key")" "$(line "$work/release.key")" >"$sums.sig"
  accepts "$rotating" "the second pinned key's line, after no signature and an unpinned key's"
  line "$work/other.key" >"$sums.sig"
  accepts "$rotating" "the first pinned key's line"
  printf 'not a signature\r\n%s\r\n' "$(line "$work/release.key")" >"$sums.sig"
  accepts "$INSTALLER" "lines ending CRLF"
  printf 'not a signature\n%s' "$(line "$work/release.key")" >"$sums.sig"
  accepts "$INSTALLER" "a last line with no newline"
  : >"$sums.sig"
  refused "$INSTALLER" "$unsigned" PATH="$ssl:$PATH"
  printf '%s\n' '-----' 'AAAA' "$(line "$work/unpinned.key")" >"$sums.sig"
  refused "$INSTALLER" "$unsigned" PATH="$ssl:$PATH"
  { line "$work/release.key"; printf '%4096s\n' ''; } >"$sums.sig"
  refused "$INSTALLER" "download failed: $BASE/download/$TAG/checksums.txt.sig" PATH="$ssl:$PATH"
  restore
fi

echo "== without OpenSSL 3, says the signature is not checked, and checks the digest alone"
mkdir -p "$work/nossl"
printf '#!/bin/sh\nexit 1\n' >"$work/nossl/openssl" # first on PATH, as LibreSSL or OpenSSL 1.1 can be
chmod +x "$work/nossl/openssl"
out="$(PATH="$work/nossl:$PATH" TERMA_RELEASE_BASE="$BASE" TERMA_VERSION="$TAG" TERMA_INSTALL_DIR="$work/bin7" sh "$INSTALLER" 2>&1)"
[ "$("$work/bin7/terma" version)" = "$want" ] || fail "install without OpenSSL 3: $out"
grep -qE "^Warning: $TAG's signature is not checked: that needs OpenSSL 3, and there is none on PATH here\. terma_[A-Za-z]+_[a-z0-9_]+\.tar\.gz is checked against checksums\.txt alone\.$" <<<"$out" \
  || fail "no warning without OpenSSL 3: $out"

echo "== refuses a release with no signature, with OpenSSL 3 or without"
rm "$sums.sig"
refused "$INSTALLER" "download failed: $BASE/download/$TAG/checksums.txt.sig"
refused "$INSTALLER" "download failed: $BASE/download/$TAG/checksums.txt.sig" PATH="$work/nossl:$PATH"
restore

echo "== reads no more than a file's bound when the server sends no length"
for f in checksums.txt.sig checksums.txt; do
  : >"$mirror/download/$TAG/$f.endless"
  refused "$INSTALLER" "download failed: $BASE/download/$TAG/$f"
  rm "$mirror/download/$TAG/$f.endless"
  sent=''
  for _ in $(seq 1 50); do
    sent="$(sed -n "s|.*streamed \([0-9]*\) bytes of .*/$f\$|\1|p" "$work/http.log" | tail -n 1)"
    [ -z "$sent" ] || break
    sleep 0.1
  done
  [ -n "$sent" ] && [ "$sent" -lt $((32 << 20)) ] || fail "$f: the server sent ${sent:-nothing} bytes"
done

echo "== refuses a checksum that does not match, signed or not"
sed 's/^[0-9a-f]\{4\}/dead/' "$work/checksums.good" >"$sums"
sign "$TAG"
refused "$INSTALLER" "checksum mismatch"
refused "$INSTALLER" "checksum mismatch" PATH="$work/nossl:$PATH"
restore

echo "== refuses a version that is not a release tag"
refused "$INSTALLER" "not a release: v1.0.0/../x" TERMA_VERSION=1.0.0/../x

echo "== cleartext only to 127.0.0.1, localhost or [::1], with at most a port"
# loopback <url> runs install.sh's loopback_http on url.
# shellcheck disable=SC1090 # the installer, sourced for its functions
loopback() { (TERMA_INSTALL_FUNCTIONS_ONLY=1 . "$RELEASED" && loopback_http "$1"); }
for url in http://127.0.0.1 http://127.0.0.1/r "http://127.0.0.1:$PORT/r" http://localhost:80/r 'http://[::1]/r' 'http://[::1]:8080/r'; do
  loopback "$url" || fail "$url is loopback"
done
for url in https://127.0.0.1/r http://127.0.0.1.example/r http://127.0.0.1:/r 'http://127.0.0.1:80?x/r' \
  "http://localhost:1@127.0.0.1:$PORT/r" http://127.0.0.1:80@example.invalid/r 'http://[::1]@example.invalid/r' \
  http://example.invalid/r; do
  if loopback "$url"; then fail "$url is not loopback"; fi
done
refused "$INSTALLER" "download failed" TERMA_RELEASE_BASE="http://localhost:1@127.0.0.1:$PORT/miradorlabs/terma-cli/releases"

echo "== refuses cleartext to a host that is not loopback"
refused "$INSTALLER" "download failed" TERMA_RELEASE_BASE="http://example.invalid/releases"

echo "ok: install.sh ($(grep -c GET "$work/http.log") requests served)"
