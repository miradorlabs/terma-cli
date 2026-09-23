#!/bin/sh
# Installs the latest terma release (or TERMA_VERSION=vX.Y.Z) on macOS and Linux.
#
#   curl -fsSL https://terma.ai/install.sh | bash
#
# The script is POSIX sh, so `| sh` works too. What it does, so you can read it
# before you run it: pick the archive for this platform from GitHub Releases, verify
# it against the release's checksums.txt, extract the single `terma` binary, and
# place it on your PATH. Nothing else is written; nothing downloaded is executed
# before it has been verified. Windows users: download terma_Windows_x86_64.zip
# from https://github.com/miradorlabs/terma-cli/releases.
#
#   TERMA_VERSION      install this release instead of the latest (v1.2.3 or 1.2.3)
#   TERMA_INSTALL_DIR  put the binary here instead of /usr/local/bin or ~/.local/bin
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

# Where to put it: an explicit TERMA_INSTALL_DIR, else /usr/local/bin when writable
# (with sudo when it is not and a terminal is attached), else ~/.local/bin. Under
# `curl | sh` stdin is the pipe, so the terminal check is on stderr; sudo prompts
# on /dev/tty, not stdin.
dest="${TERMA_INSTALL_DIR:-}"
sudo_cmd=""
if [ -z "$dest" ]; then
  if [ -w /usr/local/bin ]; then
    dest=/usr/local/bin
  elif [ -t 2 ] && command -v sudo >/dev/null 2>&1; then
    dest=/usr/local/bin; sudo_cmd="sudo"
    say "Installing to ${dest} needs sudo (set TERMA_INSTALL_DIR to install elsewhere)."
  else
    dest="$HOME/.local/bin"
  fi
fi
$sudo_cmd mkdir -p "$dest"
$sudo_cmd install -m 0755 "$tmp/terma" "$dest/terma"

# No quarantine handling is needed here: curl does not set com.apple.quarantine,
# only browser downloads do. The Homebrew cask strips it because Homebrew sets it.

say "Installed $("$dest/terma" version 2>/dev/null || echo terma) to ${dest}/terma"
case ":$PATH:" in
  *":$dest:"*) ;;
  *) say "Note: ${dest} is not on your PATH. Add it, e.g.:  export PATH=\"${dest}:\$PATH\"" ;;
esac
say "Next: run \`terma setup\` in a terminal, then \`terma install\` inside a repository."
