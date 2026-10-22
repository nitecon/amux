# amux

Agent multiplexer for highly autonomous agent orchestration on a single or multiple machines.

amux is a full rebuild of CMUX for Linux, Windows, and macOS. It runs the
terminals and agent processes on each machine, keeps the useful native CMUX
capabilities, and uses agent-gateway as the authoritative
task store and router between machines. Agents can hand work to agents on other
machines, and that work survives the destination being offline or rebooting.

Status: first native implementation candidate; the full MVP remains in progress.
The provisional Rust/Iced terminal has local workspaces, tabs/splits, explicit
Codex/Claude launches with separate session identities, layout recovery without
automatic reruns, and a gateway task CLI/viewer. Safe automatic reminders,
complete lifecycle/ownership acceptance and native Windows validation are pending.
This is not complete CMUX parity or a final stack selection.

With Rust 1.96.1 installed:

```sh
cargo run -- --project-dir /path/to/project desktop
cargo run -- agent codex
cargo run -- whoami
cargo test --no-default-features --tests
```

`whoami` runs inside an amux-launched agent and prints that execution's inherited
identity. `cargo run -- agents` lists live launches. Provider commands must already
be on PATH. See [native review instructions](docs/windows-review.md) for bundles,
gateway configuration, platform limitations and feedback requirements.

## Documentation

- [Architecture](docs/architecture.md): local amux vs. gateway responsibilities,
  cross-machine handoff, and owner decisions.
- [Features](docs/features.md): CMUX parity baseline, agent capabilities, scope,
  and acceptance scenarios.
- [Tooling](docs/tooling.md): language, terminal engine, and UI candidates, and
  how they are evaluated.
