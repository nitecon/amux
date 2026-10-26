#!/bin/sh
# Build amux.app from release binaries. Unsigned (ad-hoc) by default; set the
# MAS_* variables to sign for the Mac App Store and produce an installer .pkg.
#
#   scripts/package_macos.sh <binary-dir> <output-dir>
#
# Optional environment:
#   MACOS_BUNDLE_ID          bundle identifier (default com.nitecon.amux)
#   MAS_APP_IDENTITY         "3rd Party Mac Developer Application: ..." keychain identity
#   MAS_INSTALLER_IDENTITY   "3rd Party Mac Developer Installer: ..." keychain identity
#   MAS_PROVISIONING_PROFILE path to the App Store provisioning profile
set -eu

bin=$1
out=$2
root=$(cd "$(dirname "$0")/.." && pwd)
version=$(sed -n 's/^version = "\(.*\)"/\1/p' "$root/Cargo.toml" | head -n 1)
bundle_id=${MACOS_BUNDLE_ID:-com.nitecon.amux}
app="$out/amux.app"

rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
sed -e "s/__VERSION__/$version/g" -e "s/__BUNDLE_ID__/$bundle_id/g" \
    "$root/packaging/macos/Info.plist" > "$app/Contents/Info.plist"
# The desktop finds the CLI beside itself, so both live in Contents/MacOS.
cp "$bin/amux-desktop" "$bin/amux" "$app/Contents/MacOS/"
cp "$root/LICENSE" "$app/Contents/Resources/LICENSE"

iconset="$out/amux.iconset"
rm -rf "$iconset"
mkdir -p "$iconset"
for size in 16 32 128 256 512; do
    sips -z "$size" "$size" "$root/assets/icons/amux.png" --out "$iconset/icon_${size}x${size}.png" >/dev/null
    double=$((size * 2))
    sips -z "$double" "$double" "$root/assets/icons/amux.png" --out "$iconset/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/amux.icns"
rm -rf "$iconset"

if [ -n "${MAS_APP_IDENTITY:-}" ]; then
    : "${MAS_INSTALLER_IDENTITY:?set MAS_INSTALLER_IDENTITY to build the App Store package}"
    : "${MAS_PROVISIONING_PROFILE:?set MAS_PROVISIONING_PROFILE to build the App Store package}"
    cp "$MAS_PROVISIONING_PROFILE" "$app/Contents/embedded.provisionprofile"
    # Sign the nested CLI first, then the app that seals it.
    codesign --force --timestamp --sign "$MAS_APP_IDENTITY" \
        --entitlements "$root/packaging/macos/cli.entitlements" "$app/Contents/MacOS/amux"
    codesign --force --timestamp --sign "$MAS_APP_IDENTITY" \
        --entitlements "$root/packaging/macos/amux.entitlements" "$app"
    codesign --verify --strict --verbose=2 "$app"
    productbuild --component "$app" /Applications --sign "$MAS_INSTALLER_IDENTITY" \
        "$out/amux-macos-v$version.pkg"
else
    # Ad-hoc signature so the unsigned build still launches on Apple Silicon.
    codesign --force --sign - "$app/Contents/MacOS/amux"
    codesign --force --sign - "$app"
fi

ditto -c -k --keepParent "$app" "$out/amux-macos-aarch64-v$version.zip"
echo "$app"
