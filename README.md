# amux

Agent multiplexer for highly autonomous agent orchestration on a single or multiple machines.

amux is a full rebuild of CMUX for Linux, Windows, and macOS. It runs the
terminals and agent processes on each machine, keeps the useful native CMUX
capabilities, and uses agent-gateway as the authoritative
task store and router between machines. Agents can hand work to agents on other
machines, and that work survives the destination being offline or rebooting.

Portable Linux and Windows builds are available in [tagged releases](https://github.com/nitecon/amux/releases/latest).
Status: early release; the MVP is not complete.
The provisional Rust/Iced terminal has local workspaces, tabs/splits, explicit
Codex/Claude launches with separate session identities, layout recovery with automatic terminal reopening and native agent continuation, gateway task and message CLIs, and fresh gateway task notifications delivered
to matching local agent sessions. Busy agents and unfinished drafts keep notifications
queued. Full lifecycle acceptance with installed providers and native Windows review
remain pending. See the [MVP plan](docs/mvp.md).

With Rust 1.96.1 installed:

```sh
cargo build --bins                                   # amux (CLI) and amux-desktop (GUI)
target/debug/amux-desktop --project-dir /path/to/project
cargo run -- agent codex
cargo run -- whoami
cargo test --no-default-features --tests
```

`amux-desktop` opens the desktop without a console window on Windows. Agent tabs
always run the `amux` CLI next to it, so keep both binaries together;
`amux --project-dir ... desktop` opens the same desktop from a console.

Open **Preferences** for a separate window with **Gateway** and **Updates** tabs.
Enter the gateway URL and API key, then save. Each workspace resolves its project
automatically from its Git remote and the gateway’s canonical repository mapping.
Preferences persist on this device; no project identifier or environment setup
is required for the desktop. Enable **Send fresh gateway task notifications to agents
in this app** to allow terminal delivery; machine and session identity are created
automatically. The task panel shows connection status, agents, pending notifications
and recent delivery receipts. Offline history is never replayed into terminals.
Each Terminal, Codex and Claude tab has its own **×** close button and a directory
subtitle; hover the subtitle for the full path and terminal title.

The top-right bell shows app updates. Direct-download installs use
**Update now** in the Updates tab to download and replace both executables;
package-managed installs show their own upgrade instructions.
Restart is explicit and reopens previously running tabs in place. See [app updates](docs/updates.md).

`whoami` prints the calling agent’s project UUID, terminal agent name and execution
identity. The desktop registers agents started from shell tabs as well as its
Codex and Claude buttons. `amux agents` lists live local agents. Provider commands must already
be on PATH. See [native review instructions](docs/windows-review.md) for bundles,
gateway configuration, platform limitations and feedback requirements.

Launch the desktop without arguments to open **Default**, a terminal in your home directory.
Use **New workspace** to select an existing directory (**General**), clone a Git repository
into a chosen parent folder (**Clone**), or start an interactive initialization script
from home (**Script**). General also accepts an optional initialization script.
The dialog includes a folder and script browser; paths can also be entered directly.
Unix scripts use POSIX shell; Windows supports PowerShell `.ps1` and `.cmd`/`.bat`.
Scripts run only on an explicit terminal launch, including new tabs and splits;
restoring a saved layout never reruns them. Git cloning requires Git and configured
repository access; failed clones keep their partial folder for inspection.

Use the **×** beside a workspace to close it. Running terminals require confirmation;
project folders and files stay on disk. Closed workspaces stay removed after restart.
Closing the last workspace opens a fresh **Default** terminal in your home directory.

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
- [Gateway delivery](docs/gateway-delivery.md): automatic notifications, session identity
  and built-in terminal agent identity.
- [Reminder validation](docs/reminder-validation.md): how legacy selected-task reminders are
  gated and tested.
- [Native review](docs/windows-review.md): bundle checks for Linux and Windows
  reviewers.
