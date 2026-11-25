# Build from source

Use Go 1.26.11 or newer and run the commands from the repository root. The desktop uses Fyne and requires a C compiler and platform graphics dependencies. See the [packaging workflow](../.github/workflows/go-packaging.yml) for the Linux packages and Windows UCRT64 compiler used in CI. On macOS, install the Xcode command-line tools.

## Build and run

```sh
make build
./bin/amux-desktop
```

On Windows, use `bin\amux-desktop.exe`. Keep the desktop next to the CLI and bundled tools.

- `make cli` builds the display-free CLI for a server or headless worker.
- `make desktop` builds the GUI and stages the pinned MCP tools on supported release platforms.
- `make tools` stages the pinned tools for an existing build.

## Cross-build binaries

On macOS, install Docker and MinGW-w64 or Zig, then run `make binaries` or `./scripts/build-binaries.sh`. The script writes the macOS Apple Silicon, Linux x86-64, and Windows x86-64 binary pairs to `bin/`. Linux binaries use `.bin` and Windows binaries use `.exe`. Set `WINDOWS_CC` to select a different Windows cross-compiler.

See [gateway deployment](../deploy/README.md) for server configuration and [app updates](updates.md) for release packaging.
