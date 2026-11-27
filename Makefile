GO ?= go

ifeq ($(OS),Windows_NT)
EXE := .exe
endif

TOOL_TARGET_darwin_arm64 := aarch64-apple-darwin
TOOL_TARGET_linux_amd64 := x86_64-unknown-linux-gnu
TOOL_TARGET_windows_amd64 := x86_64-pc-windows-msvc
TOOL_TARGET = $(TOOL_TARGET_$(shell $(GO) env GOOS)_$(shell $(GO) env GOARCH))

.DEFAULT_GOAL := build

.PHONY: build cli desktop tools binaries

build: desktop

binaries:
	GO="$(GO)" ./scripts/build-binaries.sh

cli: | bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/amux$(EXE) ./cmd

desktop: $(if $(TOOL_TARGET),tools) | bin
	CGO_ENABLED=1 $(GO) build -trimpath -tags desktop -o bin/amux$(EXE) ./cmd
	cp bin/amux$(EXE) bin/amux-desktop$(EXE)

tools: | bin
	@test -n "$(TOOL_TARGET)" || { echo "No pinned MCP tool bundle for this platform"; exit 1; }
	@if [ ! -x bin/agent-tools$(EXE) ] || [ ! -x bin/memory$(EXE) ] || \
		[ ! -f bin/.bundled-tools-$(TOOL_TARGET) ] || [ packaging/bundled-tools.json -nt bin/.bundled-tools-$(TOOL_TARGET) ]; then \
		$(GO) run ./tools/amux-release tools --target $(TOOL_TARGET) --output bin && \
		touch bin/.bundled-tools-$(TOOL_TARGET); \
	fi

bin:
	mkdir -p bin
