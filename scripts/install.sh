#!/bin/sh
# Install r2backup. Downloads the release binary for this platform, verifies it
# against the published checksums, and puts it on the PATH.
set -eu

REPO="saurabhhbansal/r2backup"
INSTALL_DIR="${R2BACKUP_INSTALL_DIR:-}"

fail() { printf 'install: %s\n' "$1" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) fail "unsupported operating system: $os" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) fail "unsupported architecture: $arch" ;;
esac

# Pick an install directory that is already on the PATH where possible, so the
# user does not have to edit their shell profile before running the thing they
# just installed.
if [ -z "$INSTALL_DIR" ]; then
  if [ -w /usr/local/bin ]; then
    INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR"

command -v curl >/dev/null 2>&1 || fail "curl is required"

printf 'Finding the latest release...\n'
tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
  | sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' | head -1)
[ -n "$tag" ] || fail "could not determine the latest release"
version=${tag#v}

asset="r2backup_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

printf 'Downloading %s...\n' "$asset"
curl -fsSL "$base/$asset" -o "$tmp/$asset" || fail "download failed: $base/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || fail "could not fetch checksums.txt"

# Verify before unpacking, not after. An unverified archive should never reach
# the filesystem the user runs things from.
expected=$(grep " $asset\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$expected" ] || fail "$asset is not listed in checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
  fail "no sha256 tool found; refusing to install an unverified download"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch — refusing to install"

tar -xzf "$tmp/$asset" -C "$tmp"
install -m 0755 "$tmp/r2backup" "$INSTALL_DIR/r2backup" 2>/dev/null \
  || { cp "$tmp/r2backup" "$INSTALL_DIR/r2backup" && chmod 0755 "$INSTALL_DIR/r2backup"; }

printf '\nInstalled r2backup %s to %s\n' "$tag" "$INSTALL_DIR/r2backup"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) printf 'Note: %s is not on your PATH. Add it, or run the binary by its full path.\n' "$INSTALL_DIR" ;;
esac
printf '\nNext: r2backup setup\n'
