GO ?= go

ifeq ($(OS),Windows_NT)
EXE := .exe
endif

.DEFAULT_GOAL := build

.PHONY: build cli desktop

build: cli desktop

cli: | bin
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/amux$(EXE) ./cmd/amux

desktop: | bin
	$(GO) build -trimpath -tags desktop -o bin/amux-desktop$(EXE) ./cmd/amux-desktop

bin:
	mkdir -p bin
