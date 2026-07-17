#!/bin/sh
# ago installer: curl -fsSL https://ago.aeryx.ai/install.sh | sh
# Downloads the latest release binary for this OS/arch, installs it to
# ~/.local/bin (or $AGO_INSTALL_DIR), and installs the agent skill.
set -eu

REPO="guygrigsby/agent-go"
DIR="${AGO_INSTALL_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin|linux) ;;
  *) echo "unsupported OS: $os (Windows: download the zip from the releases page)" >&2; exit 1 ;;
esac

url="https://github.com/$REPO/releases/latest/download/ago_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "fetching $url"
curl -fsSL "$url" -o "$tmp/ago.tar.gz"
tar -xzf "$tmp/ago.tar.gz" -C "$tmp"
mkdir -p "$DIR"
install -m 0755 "$tmp/ago" "$DIR/ago"
echo "installed $DIR/ago"
"$DIR/ago" skill install
case ":$PATH:" in
  *":$DIR:"*) ;;
  *) echo "note: add $DIR to your PATH" ;;
esac
