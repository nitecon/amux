# Engineering rules

- Keep code changes inside this repository. Delegate changes owned by another project to that project.
- Keep changes simple and scoped. Check whether an existing package, interface, or API can solve the problem before adding one. When you find duplication outside the current scope, file a focused follow-up.
- Add compatibility paths or migrations only after discussing them with the user.
- Keep release tags in `vMAJOR.MINOR.PATCH` format and retain existing tags. Diagnose intermittent integration failures before making another commit.
- Use Go and shell tooling for repository work. Do not use Python.

# Go structure and startup

- Use clearly named packages outside `internal/` and avoid stuttering names. Put the public entry point in `<package>/<package>.go` by default. The CLI uses `cmd/main.go` and the provider interface and registry live in `providers/provider.go`.
- Define commands with urfave/cli v3 under `cmd`. Commands parse invocation arguments, load configuration as needed, and call the owning package's entry point. Use zerolog for application logging.
- Keep function inputs unambiguous. The owning package reads active configuration from `config` and environment values with `os.Getenv`. Callers pass inputs they own, such as context, streams, or a typed session binding.

# Configuration

- Keep client and server settings separate. Use `config.Get()` for client settings and `config.GetServer()` for server settings. Protect publication with separate mutexes and treat returned maps, slices, and pointed-to values as read-only.
- Apply settings changes by writing the file first, then calling the same loader used at startup. The loader reads and validates before publishing under its mutex. A failed load returns the error and preserves the active settings. Keep getters free of deep copies.
- Keep server runtime settings in YAML. Start the server with `amux server --config PATH` and let `server.Start` read the active settings.
- Desktop, headless clients, and stdio MCP use `$HOME/.amux/config.yaml` for gateway connectivity. Keep machine-specific settings in `$HOME/.amux/device.yaml`. Load fleet policy from the gateway's authenticated API and apply device-local limits.

# State machine

- `fism` is the gateway’s finite state machine. Register transition logic in `controllers/`. A controller can subscribe to multiple event types, and an event type can have multiple controllers. Declare a dependency when one controller needs another’s outcome.
- Keep accepted inputs, complete domain state, and committed outcomes in SQLite. Commit accepted input before acknowledging it. Keep intermediate phases, controller progress, and retry timing in Go. Guard final writes with the relevant state or revision and acknowledge the accepted input only after settlement.
- `fism` selects the active controllers for each event and dispatches them through the shared `queues` manager. Independent controllers run concurrently. Go channels report outcomes, and `fism` waits for required controllers before advancing. If the final SQLite write fails, retry the write with the outcome already in memory.
- On restart, start a fresh pass from durable accepted input and current domain state. Use stable operation identities so a controller can recognize an effect that already happened. The current registry handles events emitted by the new pass. Subscriptions never backfill controller history.
- Keep WebSocket publication and consumer cursors independent of controller execution. A reconnecting client may resume its stream from its cursor. Controller recovery is driven by accepted domain inputs.

See the [state machine design](docs/StateMachine.md) for implementation details.

# MCP and providers

- Keep stdio MCP serving and tool registration in `mcp`. Initialize the official MCP Go SDK transport from `cmd`. MCP arguments carry session-specific values. MCP reads the same home configuration as the client.
- Keep provider code under `providers/`. Register each provider in `providers/provider.go` and use the shared `Provider` and `Session` interfaces. Keep protocol details in each adapter and shared process and RPC code in `providers/process` and `providers/rpc`.
- For Go changes, run both `go build ./...` and the relevant tests. Compile affected build-tag variants and verify packaging when moving packages that the build depends on.
