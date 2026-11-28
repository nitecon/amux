#!/bin/sh
# Install or upgrade amux on macOS (Apple Silicon) from the latest GitHub release.
#   curl -fsSL https://raw.githubusercontent.com/nitecon/amux/main/install-macos.sh | sh
set -eu

repo=nitecon/amux
apps="$HOME/Applications"

[ "$(uname -s)" = Darwin ] || { echo "install-macos.sh supports macOS only; use install.sh or install.ps1" >&2; exit 1; }
[ "$(uname -m)" = arm64 ] || { echo "amux releases support Apple Silicon Macs only" >&2; exit 1; }

guide="https://github.com/$repo/blob/main/deploy/README.md#upgrade-an-existing-deployment"
echo "Upgrading? Stop the desktop, gateway, and headless clients and back up server state first. See $guide" >&2

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

# Stage and verify the whole app before touching the existing install.
ditto -x -k "$tmp/$archive" "$tmp/bundle"
[ -d "$tmp/bundle/amux.app" ] || { echo "release archive does not contain amux.app" >&2; exit 1; }
codesign --verify --strict "$tmp/bundle/amux.app"

# Swap the whole sealed app bundle; put the old one back if the swap fails.
if [ -e "$apps/amux.app" ]; then
  mv "$apps/amux.app" "$tmp/amux.app.old"
fi
if ! mv "$tmp/bundle/amux.app" "$apps/amux.app"; then
  if [ -e "$tmp/amux.app.old" ] && ! mv "$tmp/amux.app.old" "$apps/amux.app"; then
    trap - EXIT
    echo "Install failed and rollback did not finish; previous app is at $tmp/amux.app.old" >&2
    exit 1
  fi
  echo "Install failed; the previous amux install was restored." >&2
  exit 1
fi

echo "amux $tag installed. Start it with: open \"$apps/amux.app\""
echo "For a standalone server, update the service binary and start it with amux server --config PATH. See $guide"
