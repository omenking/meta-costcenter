#!/bin/sh
# Installs the latest meta-cost release into ~/.local/bin (or $META_COST_INSTALL_DIR):
#   curl -fsSL https://raw.githubusercontent.com/omenking/meta-costcenter/main/install.sh | sh
set -eu

repo=omenking/meta-costcenter
dir=${META_COST_INSTALL_DIR:-$HOME/.local/bin}

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "meta-cost runs on Linux (including WSL) and macOS; this system is $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "No meta-cost release for $(uname -m)" >&2; exit 1 ;;
esac

archive=meta-cost_${os}_$arch.tar.gz
base=https://github.com/$repo/releases/latest/download
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $archive"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
# macOS ships shasum rather than sha256sum.
if command -v sha256sum >/dev/null; then sha256="sha256sum"; else sha256="shasum -a 256"; fi
(cd "$tmp" && grep " $archive\$" checksums.txt | $sha256 -c --quiet -)
tar -xzf "$tmp/$archive" -C "$tmp" meta-cost

mkdir -p "$dir"
install -m 0755 "$tmp/meta-cost" "$dir/meta-cost"
echo "Installed $("$dir/meta-cost" --version) to $dir/meta-cost"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH, for example: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.bashrc (or ~/.zshrc on macOS)" ;;
esac
echo "Next: run 'meta-cost show' to open the dashboard."
