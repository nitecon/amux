#!/bin/sh
# Install or upgrade amux on macOS (Apple Silicon) from the latest GitHub release.
#   curl -fsSL https://raw.githubusercontent.com/nitecon/amux/main/install-macos.sh | sh
set -eu

repo=nitecon/amux
apps="$HOME/Applications"

[ "$(uname -s)" = Darwin ] || { echo "install-macos.sh supports macOS only; use install.sh or install.ps1" >&2; exit 1; }
[ "$(uname -m)" = arm64 ] || { echo "amux releases support Apple Silicon Macs only" >&2; exit 1; }

tag=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed 's|.*/||')
base="https://github.com/$repo/releases/download/$tag"
archive="amux-macos-aarch64-$tag.zip"
echo "Installing amux $tag into $apps/amux.app"

mkdir -p "$apps"
tmp=$(mktemp -d "$apps/.amux-install-XXXXXX")
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
(cd "$tmp" && grep " $archive\$" SHA256SUMS | shasum -a 256 -c -)

ditto -x -k "$tmp/$archive" "$tmp/bundle"
[ -d "$tmp/bundle/amux.app" ] || { echo "release archive does not contain amux.app" >&2; exit 1; }

# Swap the whole sealed app bundle; keep the old one until the new one is in place.
if [ -e "$apps/amux.app" ]; then
  mv "$apps/amux.app" "$tmp/amux.app.old"
fi
mv "$tmp/bundle/amux.app" "$apps/amux.app"

echo "amux $tag installed. Start it with: open \"$apps/amux.app\""
echo "If amux was already running, quit and reopen it to use the new version."
