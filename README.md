# amux

Agent multiplexer for highly autonomous agent orchestration on a single or multiple machines.

amux is a full rebuild of CMUX for Linux, Windows, and macOS. It runs the
terminals and agent processes on each machine, keeps the useful native CMUX
capabilities, and uses agent-gateway as the authoritative
task store and router between machines. Agents can hand work to agents on other
machines, and that work survives the destination being offline or rebooting.

Status: native implementation candidate; the MVP is not complete.
The provisional Rust/Iced terminal has local workspaces, tabs/splits, explicit
Codex/Claude launches with separate session identities, layout recovery without
automatic reruns, gateway task and message CLIs, and reminders to an explicitly
selected task (Linux, Codex only). Automatic task routing, full lifecycle and
ownership acceptance, and native Windows validation are pending. This is not
complete CMUX parity or a final stack selection. See the [MVP plan](docs/mvp.md).

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
- [MVP plan](docs/mvp.md): milestones, closing criteria, reviewer evidence, and
  remaining gates.
- [Gateway CLI](docs/gateway-cli.md): task and inline message commands.
- [Reminder validation](docs/reminder-validation.md): how terminal reminders are
  gated and tested.
- [Native review](docs/windows-review.md): bundle checks for Linux and Windows
  reviewers.
