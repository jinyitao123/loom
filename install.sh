#!/usr/bin/env sh
# Install the loom CLI (weave digital-avatar engine) from GitHub Releases.
#
#   curl -fsSL https://raw.githubusercontent.com/jinyitao123/loom/main/install.sh | sh
#
# Env:
#   LOOM_VERSION      release tag to install (default: latest)
#   LOOM_INSTALL_DIR  install directory (default: /usr/local/bin)
set -eu

REPO="jinyitao123/loom"
INSTALL_DIR="${LOOM_INSTALL_DIR:-/usr/local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "loom install: unsupported arch: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux | darwin) ;;
  *) echo "loom install: unsupported os: $os (use 'go install .../cmd/loom' on Windows)" >&2; exit 1 ;;
esac

version="${LOOM_VERSION:-latest}"
if [ "$version" = "latest" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | head -1 | sed -E 's/.*"([^"]+)".*/\1/')
fi
if [ -z "$version" ]; then
  echo "loom install: could not resolve a release version" >&2
  exit 1
fi

v="${version#v}"
url="https://github.com/$REPO/releases/download/$version/loom_${v}_${os}_${arch}.tar.gz"

echo "loom install: downloading $url"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$url" | tar -xz -C "$tmp"

if [ -w "$INSTALL_DIR" ]; then
  install -m 0755 "$tmp/loom" "$INSTALL_DIR/loom"
else
  echo "loom install: $INSTALL_DIR not writable, using sudo"
  sudo install -m 0755 "$tmp/loom" "$INSTALL_DIR/loom"
fi

echo "loom install: installed to $INSTALL_DIR/loom"
"$INSTALL_DIR/loom" version
