#!/bin/sh
# Installs the latest tgctl release into ~/.local/bin (override with TGCTL_BIN).
set -eu
repo=renpihq/tgctl
bin=${TGCTL_BIN:-$HOME/.local/bin}
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case $arch in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported arch $arch" >&2; exit 1 ;; esac
tag=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
[ -n "$tag" ] || { echo "no release found" >&2; exit 1; }
url="https://github.com/$repo/releases/download/$tag/tgctl_${os}_${arch}.tar.gz"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$url" -o "$tmp/tgctl.tgz"
curl -fsSL "https://github.com/$repo/releases/download/$tag/checksums.txt" | grep " tgctl_${os}_${arch}.tar.gz$" | sed "s#tgctl_${os}_${arch}.tar.gz#$tmp/tgctl.tgz#" | sha256sum -c - >/dev/null
tar -xzf "$tmp/tgctl.tgz" -C "$tmp" tgctl
mkdir -p "$bin" && install -m 755 "$tmp/tgctl" "$bin/tgctl"
echo "installed tgctl $tag to $bin/tgctl"
case ":$PATH:" in *":$bin:"*) ;; *) echo "note: $bin is not on PATH" ;; esac
