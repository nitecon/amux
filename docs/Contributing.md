# Contributing

Start with the [source build guide](SourceBuilds.md) and read the [Go coding guidelines](CodingGuidelines.md) before changing application code. The guidelines describe the architecture, test requirements, and Git workflow.

Check your changes from the repository root:

```sh
go test ./...
go vet ./...
```

Use `go test -race ./...` with a working C toolchain for race detection. The [packaging workflow](../.github/workflows/go-packaging.yml) checks native builds and upgrades on Linux, Windows, and macOS.

For rebuild work, see the [parity status](parity-status.md) and [behavioral test inventory](go-parity.json).
