#!/bin/sh
# Install or upgrade amux on Linux (x86-64) from the latest GitHub release.
#   curl -fsSL https://raw.githubusercontent.com/nitecon/amux/main/install.sh | sh
set -eu

repo=nitecon/amux
dir="$HOME/amux"

[ "$(uname -s)" = Linux ] || { echo "install.sh supports Linux only; use install-macos.sh or install.ps1" >&2; exit 1; }
[ "$(uname -m)" = x86_64 ] || { echo "amux releases support Linux x86-64 only" >&2; exit 1; }

tag=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed 's|.*/||')
base="https://github.com/$repo/releases/download/$tag"
archive="amux-linux-x86_64-$tag.tar.gz"
echo "Installing amux $tag into $dir"

mkdir -p "$dir"
tmp=$(mktemp -d "$dir/.amux-install-XXXXXX")
trap 'rm -rf "$tmp"' EXIT

curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
(cd "$tmp" && grep " $archive\$" SHA256SUMS | sha256sum -c -)

mkdir "$tmp/bundle"
tar -xzf "$tmp/$archive" -C "$tmp/bundle"

# Rename each file into place so running copies keep working until restart.
for file in "$tmp/bundle"/*; do
  mv -f "$file" "$dir/"
done

echo "amux $tag installed. Start it with: $dir/amux-desktop"
echo "If amux was already running, restart it to use the new version."
