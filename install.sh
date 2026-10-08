#!/bin/sh
# Installs the latest terma release (or TERMA_VERSION=vX.Y.Z) on macOS and Linux.
#
#   curl -fsSL https://terma.ai/install.sh | bash
#
# The script is POSIX sh, so `| sh` works too. What it does, so you can read it
# before you run it: pick the archive for this platform from GitHub Releases, verify
# it against the release's checksums.txt, extract the single `terma` binary, and
# place it in ~/.local/bin; it never asks for sudo. When that directory is not on
# PATH, it appends the one line that adds it to your login shell's startup file
# (~/.zshrc, ~/.bashrc or ~/.bash_profile, or a fish conf.d file). Nothing else is
# written; nothing downloaded is executed before it has been verified. Windows users:
# download terma_Windows_x86_64.zip from https://github.com/miradorlabs/terma-cli/releases.
#
#   TERMA_VERSION         install this release instead of the latest (v1.2.3 or 1.2.3)
#   TERMA_INSTALL_DIR     put the binary here instead of ~/.local/bin
#   TERMA_NO_MODIFY_PATH  set to leave your shell's startup file alone
set -eu

REPO="miradorlabs/terma-cli"
# TERMA_RELEASE_BASE exists for .github/workflows/ci.yml, which serves a snapshot
# build over loopback. Only https is accepted unless the base is loopback.
BASE="${TERMA_RELEASE_BASE:-https://github.com/${REPO}/releases}"

say() { printf '%s\n' "$*" >&2; }
die() { say "install.sh: $*"; exit 1; }

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
case "$dest" in /*) ;; *) dest="$(pwd)/$dest" ;; esac # a PATH entry must be absolute
mkdir -p "$dest" || die "cannot create ${dest} (set TERMA_INSTALL_DIR to install elsewhere)"
# Staged beside the binary and renamed over it, so nothing ever runs a half-written terma:
# a relay or hook running the old one keeps it until it starts the new one.
staged="$dest/.terma.$$"
trap 'rm -rf "$tmp"; rm -f "$staged"' EXIT
install -m 0755 "$tmp/terma" "$staged" || die "could not write ${staged}"
mv -f "$staged" "$dest/terma" || { rm -f "$staged"; die "could not install ${dest}/terma"; }

# No quarantine handling is needed here: curl does not set com.apple.quarantine,
# only browser downloads do. The Homebrew cask strips it because Homebrew sets it.

say "Installed $("$dest/terma" version 2>/dev/null || echo terma) to ${dest}/terma"

# The startup file of the login shell ($SHELL), chosen as internal/shellrc.ShellRC
# chooses it; fails for a shell this script cannot write for.
startup_file() {
  [ -n "${HOME:-}" ] || return 1
  case "${SHELL##*/}" in
    zsh) printf '%s\n' "${ZDOTDIR:-$HOME}/.zshrc" ;;
    bash)
      # macOS terminals start login shells, which read .bash_profile and not .bashrc.
      if [ "$os" = Darwin ] && [ -e "$HOME/.bash_profile" ]; then
        printf '%s\n' "$HOME/.bash_profile"
      else
        printf '%s\n' "$HOME/.bashrc"
      fi ;;
    fish) printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/terma.fish" ;;
    *) return 1 ;;
  esac
}

# The line that puts dest first on PATH, as shellrc.RC.PathLine writes it: a directory
# under home as $HOME/…, and what stays active inside double quotes escaped.
# shellcheck disable=SC2016 # $HOME and $PATH are written for the startup file to expand
path_line() {
  prefix='' dir="$dest"
  case "$dir" in "$HOME"/*) prefix='$HOME/' dir="${dir#"$HOME"/}" ;; esac
  if [ "${SHELL##*/}" = fish ]; then
    printf 'fish_add_path --move --prepend "%s%s"\n' "$prefix" "$(printf '%s' "$dir" | sed 's/[\\$"]/\\&/g')"
  else
    printf 'export PATH="%s%s:$PATH"\n' "$prefix" "$(printf '%s' "$dir" | sed 's/[\\$"`]/\\&/g')"
  fi
}

# Appends the line once, so new terminals find terma, and prints the file. This script
# runs as a child of the user's shell, so it cannot change that shell's PATH itself.
add_to_path() {
  [ -z "${TERMA_NO_MODIFY_PATH:-}" ] || return 1
  rc="$(startup_file)" || return 1
  line="$(path_line)"
  if ! grep -qsxF "$line" "$rc"; then
    sep='' # a newline first when the file does not end in one
    if [ -s "$rc" ] && [ -n "$(tail -c 1 "$rc")" ]; then sep='\n'; fi
    mkdir -p "$(dirname "$rc")" 2>/dev/null || return 1
    printf '%b# added by the terma installer\n%s\n' "$sep" "$line" >>"$rc" 2>/dev/null || return 1
  fi
  printf '%s\n' "$rc"
}

next="\`terma setup\` in a terminal"
case ":$PATH:" in
  *":$dest:"*) ;;
  *)
    if rc="$(add_to_path)"; then
      shown="$rc"
      # shellcheck disable=SC2088 # shown to the user, not expanded
      case "$rc" in "$HOME"/*) shown="~/${rc#"$HOME"/}" ;; esac
      case "$shown" in *[!A-Za-z0-9/._~-]*) shown="'$rc'" ;; esac
      say "Added ${dest} to your PATH in ${shown}; new terminals will find terma."
      next="\`source ${shown}\` in this terminal, then \`terma setup\`"
    else
      say "Note: ${dest} is not on your PATH. Add it, e.g.:  export PATH=\"${dest}:\$PATH\""
    fi ;;
esac
say "Next: run ${next}."
