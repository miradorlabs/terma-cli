#!/bin/sh
# Installs the latest terma release (or TERMA_VERSION=vX.Y.Z) on macOS and Linux.
#
#   curl -fsSL https://terma.ai/install.sh | bash
#
# The script is POSIX sh, so `| sh` works too. What it does, so you can read it
# before you run it: pick the archive for this platform from GitHub Releases, verify
# it against the release's checksums.txt, extract the single `terma` binary, and
# place it in ~/.local/bin; it never asks for sudo. When that directory is not on
# PATH, it appends the one line that adds it to your login shell's startup files (zsh's
# .zshrc; bash's .bashrc and its login file; or a fish conf.d file). Nothing else is
# written; nothing downloaded is executed before it has been verified. Windows users:
# download terma_Windows_x86_64.zip from https://github.com/miradorlabs/terma-cli/releases.
#
#   TERMA_VERSION         install this release instead of the latest (v1.2.3 or 1.2.3)
#   TERMA_INSTALL_DIR     put the binary here instead of ~/.local/bin
#   TERMA_NO_MODIFY_PATH  set, to any value or none, to leave your shell's startup file alone
set -eu

REPO="miradorlabs/terma-cli"
# TERMA_RELEASE_BASE exists for .github/workflows/ci.yml, which serves a snapshot
# build over loopback. Only https is accepted unless the base is loopback.
BASE="${TERMA_RELEASE_BASE:-https://github.com/${REPO}/releases}"

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

# The startup files of shell $1 (zsh, bash, fish), one per line, picked as
# internal/shellrc.ShellRC picks them; fails for a shell this script cannot write for.
startup_files() {
  [ -n "${HOME:-}" ] || return 1
  case "$1" in
    zsh) printf '%s\n' "${ZDOTDIR:-$HOME}/.zshrc" ;;
    bash)
      # An interactive shell reads .bashrc. A login shell (a macOS terminal, ssh) reads
      # the first of these that exists and is readable, and need not read .bashrc, so
      # both get the line. A new .profile shadows nothing, and sh reads it too.
      printf '%s\n' "$HOME/.bashrc"
      for f in .bash_profile .bash_login .profile; do
        if [ -r "$HOME/$f" ]; then printf '%s\n' "$HOME/$f"; return 0; fi
      done
      printf '%s\n' "$HOME/.profile" ;;
    fish) printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/terma.fish" ;;
    *) return 1 ;;
  esac
}

# The line that puts directory $2 first on PATH in shell $1, as shellrc.RC.PathLine
# writes it: a directory under home as $HOME/…, and what stays active inside double
# quotes escaped.
# shellcheck disable=SC2016 # $HOME and $PATH are written for the startup file to expand
path_line() {
  prefix='' dir="$2"
  case "$dir" in "$HOME"/*) prefix='$HOME/' dir="${dir#"$HOME"/}" ;; esac
  if [ "$1" = fish ]; then
    printf 'fish_add_path --move --prepend "%s%s"\n' "$prefix" "$(printf '%s' "$dir" | sed 's/[\\$"]/\\&/g')"
  else
    printf 'export PATH="%s%s:$PATH"\n' "$prefix" "$(printf '%s' "$dir" | sed 's/[\\$"`]/\\&/g')"
  fi
}

# Appends line $2 to file $1, after a newline when the file does not end in one; fails
# quietly when the file cannot be read (nor could the shell read it) or written.
append_line() {
  [ ! -e "$1" ] || [ -r "$1" ] || return 1
  sep=''
  if [ -s "$1" ] && [ -n "$(tail -c 1 "$1")" ]; then sep='\n'; fi
  mkdir -p "$(dirname "$1")" 2>/dev/null &&
    printf '%b# added by the terma installer\n%s\n' "$sep" "$2" 2>/dev/null >>"$1"
}

# Puts line $2 in each of the files $1 names, one per line, where it is missing. Sets
# files to the ones that have it and missed to the ones it could not write, shown and
# joined, and added when any needed it.
put_line() {
  files='' missed='' added=''
  set -f; IFS='
'
  for rc in $1; do
    if grep -qsxF "$2" "$rc"; then :
    elif append_line "$rc" "$2"; then added=1
    else missed="${missed:+$missed and }$(shown "$rc")"; continue
    fi
    files="${files:+$files and }$(shown "$rc")"
  done
  set +f; unset IFS
}

# Path $1 for a command the user copies: ~/… when that needs no quoting, else the whole
# path single-quoted, as doctor.shellPath writes it.
# shellcheck disable=SC2088 # shown, not expanded
shown() {
  p="$1"
  if [ -n "${HOME:-}" ]; then case "$p" in "$HOME"/*) p="~/${p#"$HOME"/}" ;; esac; fi
  case "$p" in
    *[!A-Za-z0-9/._~-]*) printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")" ;;
    *) printf '%s' "$p" ;;
  esac
}

# internal/shellrc's tests source this script for the functions above.
[ -z "${TERMA_INSTALL_FUNCTIONS_ONLY:-}" ] || return 0

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
need curl; need tar; need uname; need mktemp

os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Darwin|Linux) ;;
  *) die "unsupported OS: $os (download a release archive from ${BASE})" ;;
esac
case "$arch" in
  x86_64|amd64) arch="x86_64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "unsupported architecture: $arch" ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  die "need sha256sum or shasum to verify the download"
fi

# Same naming as the Homebrew cask and `terma update` (internal/selfupdate.AssetName).
asset="terma_${os}_${arch}.tar.gz"
version="${TERMA_VERSION:-}"
case "$version" in
  ""|latest) release="${BASE}/latest/download" ;;
  v*)        release="${BASE}/download/${version}" ;;
  *)         release="${BASE}/download/v${version}" ;;
esac

# Cleartext is refused, including on a redirect, except from this machine.
proto='=https'
case "$BASE" in
  http://127.0.0.1:*|http://127.0.0.1/*|http://localhost:*|http://localhost/*) proto='=http,https' ;;
esac
fetch() {
  curl -fsSL --proto "$proto" --proto-redir "$proto" --tlsv1.2 --retry 3 -o "$2" "$1" \
    || die "download failed: $1"
}

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t terma)"
trap 'rm -rf "$tmp"' EXIT

say "Downloading ${asset}..."
fetch "${release}/${asset}" "$tmp/$asset"
fetch "${release}/checksums.txt" "$tmp/checksums.txt"

# Verify before extracting anything.
expected="$(grep "  ${asset}\$" "$tmp/checksums.txt" | awk '{print $1}')"
[ -n "$expected" ] || die "checksums.txt has no entry for ${asset}"
actual="$(sha256 "$tmp/$asset")"
[ "$actual" = "$expected" ] || die "checksum mismatch for ${asset} (expected ${expected}, got ${actual})"

tar -xzf "$tmp/$asset" -C "$tmp" terma || die "${asset} does not contain a terma binary"
chmod +x "$tmp/terma"

# Where to put it: an explicit TERMA_INSTALL_DIR, else ~/.local/bin. Never sudo, so
# the binary is the user's own and `terma update` can replace it.
dest="${TERMA_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dest" || die "cannot create ${dest} (set TERMA_INSTALL_DIR to install elsewhere)"
# Absolute and without `..`, as a PATH entry must be and as PATH would list it.
abs="$(CDPATH='' cd -- "$dest" && pwd)" || die "cannot use ${dest}"
dest="$abs"
# Staged beside the binary and renamed over it, so nothing ever runs a half-written terma:
# a relay or hook running the old one keeps it until it starts the new one.
staged="$dest/.terma.$$"
trap 'rm -rf "$tmp"; rm -f "$staged"' EXIT
install -m 0755 "$tmp/terma" "$staged" || die "could not write ${staged}"
mv -f "$staged" "$dest/terma" || { rm -f "$staged"; die "could not install ${dest}/terma"; }

# No quarantine handling is needed here: curl does not set com.apple.quarantine,
# only browser downloads do. The Homebrew cask strips it because Homebrew sets it.

say "Installed $("$dest/terma" version 2>/dev/null || echo terma) to ${dest}/terma"

# This script runs as a child of the user's shell, so it cannot change that shell's
# PATH: it puts dest on PATH for the terminals that start next, with a line in the
# login shell's startup files, written once, and gives the full path to run now. The
# login shell ($SHELL) is the only shell it can know: a terminal set to run another
# needs that shell's own line.
shell="${SHELL:-}"
shell="${shell##*/}"
next="\`$(shown "$dest/terma") setup\`"
case ":$PATH:" in
  *":$dest:"*) next="\`terma setup\`" ;;
  *)
    files='' missed='' added=''
    case "$dest" in
      *:*) ;; # PATH would split it at the colon, so no line can put it there
      *)
        if [ -z "${TERMA_NO_MODIFY_PATH+set}" ] && rcs="$(startup_files "$shell")"; then
          put_line "$rcs" "$(path_line "$shell" "$dest")"
        fi ;;
    esac
    if [ -n "$files" ] && [ -n "$missed" ]; then
      say "${dest} is on PATH in ${files}, for ${shell} (your login shell), but ${missed} could not be written: ${shell} terminals that read it instead will not find terma."
    elif [ -n "$added" ]; then
      say "Added ${dest} to PATH in ${files}, for ${shell} (your login shell): new ${shell} terminals will find terma."
    elif [ -n "$files" ]; then
      say "${dest} is already on PATH in new ${shell} terminals (${files})."
    else
      case "$dest" in
        *:*) say "Note: ${dest} cannot go on PATH, which would split it at the ':'." ;;
        *) say "Note: ${dest} is not on your PATH. Add it, e.g.:  export PATH=\"${dest}:\$PATH\"" ;;
      esac
    fi ;;
esac
say "Next: run ${next}."
