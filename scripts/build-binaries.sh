#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
go_command=${GO:-go}
[[ $("$go_command" env GOHOSTOS) == darwin ]] || {
    echo 'Run this build on macOS to compile the native Apple Silicon desktop.' >&2
    exit 1
}
command -v docker >/dev/null
windows_cc=${WINDOWS_CC:-}
if [[ -z $windows_cc ]]; then
    if command -v x86_64-w64-mingw32-gcc >/dev/null; then
        windows_cc=x86_64-w64-mingw32-gcc
    else
        command -v zig >/dev/null || {
            echo 'Install MinGW-w64 or Zig, or set WINDOWS_CC.' >&2
            exit 1
        }
        windows_cc='zig cc -target x86_64-windows-gnu'
    fi
fi
mkdir -p bin
stage=$(mktemp -d "$PWD/bin/.build-binaries.XXXXXX")
trap 'rm -rf -- "$stage"' EXIT

build_cli() {
    GOOS="$1" GOARCH="$2" CGO_ENABLED=0 \
        "$go_command" build -trimpath -o "$stage/amux$3" ./cmd/amux
}

echo 'Building macOS arm64'
build_cli darwin arm64 ""
GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 \
    "$go_command" build -trimpath -tags desktop -o "$stage/amux-desktop" ./cmd/amux-desktop

echo 'Building Windows amd64'
build_cli windows amd64 .exe
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC="$windows_cc" \
    "$go_command" build -trimpath -tags desktop -o "$stage/amux-desktop.exe" ./cmd/amux-desktop

echo 'Building Linux amd64'
build_cli linux amd64 .bin
go_version=$(awk '$1 == "go" {print $2; exit}' go.mod)
docker_args=(--rm --platform linux/amd64 -v "$PWD:/src:ro" -v "$stage:/out" -w /src)
module_cache=$("$go_command" env GOMODCACHE)
if [[ -d $module_cache ]]; then
    docker_args+=(-v "$module_cache:/modules:ro" -e 'GOPROXY=file:///modules/cache/download,https://proxy.golang.org')
fi
docker run "${docker_args[@]}" "golang:$go_version-bookworm" sh -ec '
    apt-get update -qq
    apt-get install -y -qq gcc libgl1-mesa-dev libwayland-dev libxkbcommon-dev xorg-dev >/tmp/amux-build-deps.log
    CGO_ENABLED=1 go build -buildvcs=false -trimpath -tags desktop -o /out/amux-desktop.bin ./cmd/amux-desktop
    /out/amux.bin client run --help >/dev/null 2>&1
    /out/amux-desktop.bin --help >/dev/null 2>&1
'

for artifact in "$stage"/amux*; do
    mv -f -- "$artifact" "bin/${artifact##*/}"
done
printf 'Built amux and amux-desktop for macOS, Linux, and Windows in %s/bin\n' "$PWD"
