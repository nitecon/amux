# amux Go Coding Guidelines

This guide defines the Go rebuild of amux. Preserve existing amux behavior unless the requirements below explicitly replace it. The existing Rust implementation and its behavioral tests are the migration reference, not a runtime dependency of the finished Go application.

## Mandatory refactor acceptance criteria

YAGNI, DRY, and KISS are requirements of the entire rebuild and every implementation wave, not optional cleanup after completion.

- **YAGNI:** Implement source parity or an explicit user requirement. Do not add speculative features, extension points, dependencies, or frameworks. Fundamental security, core data structures, and structural boundaries that are hard to change later still require appropriate design.
- **DRY:** Inspect existing implementations before adding another path. Reuse established logic where behavior and ownership match. File a focused review task for confirmed repetition and consolidate it when validated; do not expand unrelated work into a broad rewrite.
- **KISS:** Choose the simplest implementation that preserves the required behavior. Remove redundant state and indirection. Do not remove necessary correctness, identity, durability, or recovery behavior merely to make code shorter.

Before closing a task or wave, record the actual problem and requirement, existing logic inspected for reuse, the outcome of the duplication/complexity review, and relevant validation. Explain any necessary complexity using a concrete behavior or invariant. Final rebuild acceptance must include this review; passing tests alone does not establish adherence.

## Required architecture

- Use Go for the application, `github.com/coder/websocket` for WebSockets, and `fyne.io/fyne/v2` for the desktop.
- Put executable entry points and CLI command handling under `cmd/`. Domain packages must not parse process arguments or terminate the process.
- Prefer the standard library. Use `net/http` handlers; chi (`github.com/go-chi/chi/v5`) is an approved option when routing requires it.
- Use focused top-level packages. Do not introduce an `internal/` directory, generic framework, or catch-all utility package.
- Follow KISS and YAGNI. Reuse existing solutions after inspecting them. Record repeated code as a focused review task rather than expanding unrelated work.
- Use a shared Go channel event bus for plugin subscriptions. Each plugin receives its own subscription channel; plugins must not compete to read from one work queue. Bound subscription queues, define overflow/replay behavior explicitly, and own unsubscribe and shutdown.

## Runtime modes

Provide three independently usable modes:

1. **Server:** owns durable shared state, authentication, task coordination, and event streaming. Runs without a desktop, display server, interactive login, or provider worker. Support foreground execution under systemd and in a container. Configuration and credentials must work without a desktop keychain or GUI prompt.
2. **Headless client:** connects to the server, runs provider agents, receives task assignments, and publishes conversation and execution events. It has the same worker capabilities as the desktop and requires no graphical session. A remote console-only machine must be a supported worker host.
3. **Desktop:** uses Fyne for native UI, connects to the server stream, and runs the same worker engine as the headless client. UI code presents state and issues commands; it must not implement a separate scheduler or task store.

Server and headless client builds must not import Fyne or require graphical libraries. Desktop convenience startup may compose these services, but server correctness must never depend on desktop startup. Mode-specific configuration must identify the state directory and server endpoint explicitly.

A disconnected UI must not stop server or worker execution. Network interruptions must not silently lose assignments or conversation events: reconnect using durable identity and replay/resynchronization. Do not promise remote coordination while a worker cannot reach its server; preserve work and reconcile when connectivity returns. Test duplicate delivery and ownership conflicts during reconnection.

## Conversations and provider integration

Replace embedded terminals and terminal injection with structured provider integrations. A provider subprocess using documented stdio protocol is acceptable; a PTY, simulated typing, or scraping terminal output is not.

- Support Antigravity through Agent Client Protocol (ACP). Verify the supported command, protocol version, authentication, and capabilities against the installed provider and official documentation before implementing an adapter.
- Use documented direct interfaces for Claude and Codex. Verify their actual bidirectional protocols; do not assume all providers speak ACP or replace coding-agent functionality with a text-completion API.
- Keep provider transport and protocol details inside provider packages. Expose only the capabilities the shared worker actually needs. Unsupported capabilities must produce explicit errors.
- Preserve task-to-conversation association, native provider session IDs, execution identity, resume behavior, streamed text, tool calls/results, cancellation, and permission requests.
- Present chat messages and collapsed tool calls in the desktop. Tool details remain inspectable. Permission requests require an explicit response and must work through the same shared command path for desktop and headless operation.
- Persist conversation events with ordering and stable identities. Retries must not create duplicate messages or execute a task twice.
- Do not log provider credentials, authorization headers, or secrets in tool payloads.

The native MCP surface currently has 16 tools: 15 server task/message tools
(including `project_message_thread`) and local `submit_work`. A separate
bundled catalog contains 12 non-task tools backed by pinned `agent-tools` and
`memory` executables. Keep those counts and their boundaries explicit when
documenting parity. The stdio transport limits inbound frames to 4 MiB;
ordinary trusted local tool results must retain full source-compatible output,
with no 5 MiB output cap. The inbound guard remains in force.

Prefer an official SDK when it implements the required provider protocol and meets dependency review. Otherwise use the documented protocol with the standard library. Do not introduce unrelated cloud SDKs merely because they exist.

## Application lifecycle and concurrency

Construct dependencies explicitly. Constructors return errors; packages must not call `log.Fatal`, `os.Exit`, or panic for normal failures. Only executable entry points choose process exit status.

Use `signal.NotifyContext` for process shutdown. Pass `context.Context` through network, storage, provider, and worker operations. Cancel child work, stop accepting assignments, drain or persist pending events, close connections, and wait for owned goroutines within a bounded shutdown period. Return unexpected background failures to the runtime owner.

Every goroutine needs an owner and a stopping condition. Bound queues, payload sizes, and in-flight work. Never hold a shared lock across network or provider I/O. Test shutdown and cancellation rather than relying on sleeps.

Publish state-change notifications after the transaction commits. Plugins reconcile authoritative state through channel events and periodic `time.Ticker` events; ticks handle time-based transitions and recover missed wake-ups without bypassing transactional ownership or state validation. Preserve the Rust clock's coarse 30-second default and skip missed ticks. The runtime owns and stops the ticker. Channel delivery is not a replacement for durable event replay or retry obligations.

## Logging and observability

Use standard-library `log/slog` with structured fields. Include relevant project, task, conversation, agent/session, and request identities. Use RFC 3339 timestamps and configurable levels. Keep protocol stdout separate from diagnostics on stderr.

Measure meaningful latency, traffic, errors, and saturation where needed. Add Prometheus or OpenTelemetry only for an actual requirement; neither is a mandatory dependency. Do not make unmeasured throughput claims.

## HTTP and WebSocket transport

Use `http.Handler` boundaries and inject services into handlers. Apply authentication and authorization before upgrading WebSockets and before executing each command. Preserve amux's existing enrollment and credential semantics while porting them; do not invent a new ABAC/RBAC hierarchy or JWT system.

Use `github.com/coder/websocket` throughout. Associate each connection with authenticated identity and authorized subscriptions. Check origins where applicable. Avoid credentials in URL query strings. Remote credentials require protected transport.

Keep reads active for control frames; impose read limits, write deadlines, cancellation, and bounded per-subscriber delivery. A slow subscriber must not block unrelated clients or grow memory without limit. Define resynchronization when a subscriber falls behind. Close and unregister connections on all exit paths.

Use push events for live state. Reconnect with bounded backoff and cancellation; polling is only a fallback when a backend has no push support. Events must preserve the existing wire contract until an intentional, tested replacement is defined.

Validate requests at the boundary. Return consistent structured errors with appropriate HTTP status codes. Do not leak internal errors or database details to clients. Preserve existing response envelopes and command semantics through contract tests.

## Configuration

Load typed configuration once at startup using standard-library flags, environment, and JSON where sufficient. Validate required fields and reject invalid values with useful errors. Keep secrets out of committed configuration. Use restrictive permissions for credential and state files.

Separate device-local preferences from server-owned settings. Preserve existing precedence and scoping through tests. Publish server setting changes through the event stream; define when a running worker applies changes. Avoid global mutable configuration.

## Storage and domain boundaries

Use domain types without database-driver wrappers at service boundaries. Put SQL and driver-specific conversions in storage packages. Use small consumer-owned interfaces only where a real boundary needs one; do not create an interface for every struct.

Preserve amux data, identifiers, ownership rules, ordering, settings, and history. Inspect the existing schema and migration code before selecting a Go driver or changing storage. Do not replace durable state with in-memory maps as a production shortcut.

Use parameterized SQL, context-aware operations, and transactions for invariants spanning multiple writes. Version migrations and test upgrade paths with representative existing data. Back up before destructive migration; never silently drop user state.

Do not add a cache, distributed queue, cloud service, or extra database backend without a current requirement. If caching is needed, define invalidation and consistency first. Persist task scheduling and ownership changes before announcing them to workers.

## Desktop state

Fyne widgets render shared application state and send service commands. Keep network and provider work off the UI thread; marshal widget changes using the supported Fyne UI scheduling API. Do not mutate widgets from background goroutines.

Preserve project discovery, favorites/filtering, task details, notifications, settings, and existing task workflows while replacing the terminal surface with task-linked chat. Keep local presentation state separate from authoritative server data. Reconnection must preserve unsaved user input.

## Dependency review

Before adding a non-stdlib module, establish its immediate purpose, official provenance, maintenance status, supported Go versions, license, and security advisories. Pin versions in `go.mod`/`go.sum`; review transitive dependencies. Run `govulncheck` on the implemented packages and address applicable findings. Required libraries still undergo this review.

The required dependencies are coder/websocket and Fyne. chi is permitted when needed. Logger, cache, router, validation, queue, metrics, and configuration frameworks are not automatic additions.

## Test-driven migration and acceptance

Delegate most implementation work to Luna (`gpt-6-luna`) subagents with bounded, disjoint file ownership. The coordinating agent owns architecture, integration, and review; subagents work in this repository without creating worktrees or changing other codebases.

Port behavior tests before the corresponding implementation. Trace tests to Rust source/test names so parity can be audited. Keep external contracts and meaningful edge cases; do not mechanically translate terminal implementation details into the new architecture.

Classify every existing test as preserved behavior, intentionally replaced terminal behavior, or platform-specific coverage requiring an equivalent. Use a migration inventory to track outstanding features; a handful of passing Go tests is not proof of parity.

Use table-driven tests, `t.TempDir`, `httptest`, fake provider processes, and deterministic synchronization. Test real serialization and transport boundaries. Cover authentication, task ownership, FIFO assignment, notifications, replay, cancellation, migration, provider resume, and submit-work behavior. Cover permission round trips and collapsed tool rendering with provider fixtures.

For each ported area run `gofmt`, `go vet`, and focused tests, then `go test -race` where supported. Separately verify non-GUI server/headless builds with CGO disabled, the Fyne desktop build and widget behavior, and OS-specific lifecycle behavior. Test server and headless client as separate processes with no display or interactive login.

Remove superseded Rust code and tooling as replacement coverage becomes authoritative. The finished application must build, package, and run without Rust or terminal injection. Do not delete the reference behavior before it has been accounted for. Do not retain a Rust runtime fallback in the final product.

## Git and scope discipline

Work directly on the authorized rebuild branch. Do not modify another codebase. Delegate work outside this repository to its owning project. Never delete tags; new tags must use semantic versioning (`v1.2.3`). Do not add attribution trailers or change commit authors. Keep unrelated fixes and speculative features out of migration changes.
