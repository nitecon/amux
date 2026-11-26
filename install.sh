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

# Stage and verify the whole bundle before touching the existing install.
mkdir "$tmp/bundle" "$tmp/old"
tar -xzf "$tmp/$archive" -C "$tmp/bundle"
(cd "$tmp/bundle" && sha256sum -c --quiet SHA256SUMS)

# Rename each bundle file into place, moving any existing copy aside first.
# Running copies keep working until restart; other files in $dir are untouched.
installed=""
replace() {
  for file in "$tmp/bundle"/*; do
    name=${file##*/}
    if [ -e "$dir/$name" ]; then mv -f "$dir/$name" "$tmp/old/" || return 1; fi
    mv -f "$file" "$dir/" || return 1
    installed="$installed $name"
  done
}
rollback() {
  for name in $installed; do rm -f "$dir/$name" || return 1; done
  for file in "$tmp/old"/*; do
    [ -e "$file" ] || continue
    mv -f "$file" "$dir/" || return 1
  done
}

if ! replace; then
  if rollback; then
    echo "Install failed; the previous amux install was restored." >&2
  else
    trap - EXIT
    echo "Install failed and rollback did not finish; previous files are in $tmp/old" >&2
  fi
  exit 1
fi

echo "amux $tag installed. Start it with: $dir/amux-desktop"
echo "If amux was already running, restart it to use the new version."
