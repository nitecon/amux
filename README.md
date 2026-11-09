# amux

**A desktop app for running a team of Codex and Claude agents across Linux, Windows, and macOS.**

Add your repositories, launch agents, and give the team a task queue. amux manages
projects and routes work to idle agents on one machine or across several. Each
agent runs in a live terminal you can watch, join, and talk to directly.

## Why amux?

- **Work on several tasks at once.** Launch Codex and Claude for the same project.
  The amux server assigns eligible queued work to idle agents, so your agents can
  tackle separate tasks in parallel like a team of developers.
- **Use the machines you actually test on.** Open the same repository on Linux,
  Windows, and macOS, connect to the same server, and launch agents on each.
  OS requirements route platform-specific work to an agent on that OS.
- **Stay in the conversation.** amux runs the interactive Codex and Claude
  terminals and injects task instructions on your behalf. You can see the current
  execution, jump into an ongoing task, or start a conversation with any agent.
  Busy agents and unfinished drafts defer delivery.
- **Keep implementation and review in one workflow.** Completed work can queue a
  code review followed by a documentation review. Reviewers report findings and
  create correction tasks; explicit **User review** keeps decisions with you.
- **Spend context on the problem.** The bundled MCP server gives agents targeted
  code search, symbol lookup, Markdown sections, knowledge-graph lookups, task
  tools, and durable memory. Relevant memories are injected automatically, and
  idle memory review asks agents to preserve useful lessons. With agent-memory
  synchronization configured, those lessons can be shared across machines and
  operating systems.

amux is a Rust/Iced rebuild of CMUX with native terminals, workspaces, tabs and
splits, plus a server that keeps task coordination durable across disconnects and
reboots. It is an early release; the MVP is not complete. See the
[MVP plan](docs/mvp.md) for remaining acceptance gates.

## Get the desktop app

Download the complete bundle for your machine from
[tagged releases](https://github.com/nitecon/amux/releases/latest).
See the [changelog](CHANGELOG.md) for release changes.

| Platform | Build | Launch |
| --- | --- | --- |
| Linux | x86-64 portable bundle | `./amux-desktop` |
| Windows | x86-64 portable bundle | `amux-desktop.exe` |
| macOS | Apple Silicon app preview, unsigned | Open `amux.app` |

Extract the whole bundle and keep its files together, including the CLI, desktop,
bundled tools and build metadata. On Windows, `amux-desktop.exe` opens without a
console window. macOS first-launch instructions and signing limitations are in
[macOS builds](docs/macos-app-store.md#unsigned-builds-current).

Install Codex and/or Claude separately, authenticate them, and make their commands
available on your PATH. Git and configured repository access are needed to clone
projects. amux bundles pinned builds of
[agent-tools](https://github.com/nitecon/agent-tools) and
[agent-memory](https://github.com/nitecon/agent-memory).

## From a repository to a team

1. **Add a project.** Launch amux and use **New workspace** to select your local
   repository (**General**) or clone one (**Clone**). Projects are matched by Git
   remote, so checkouts of the same repository resolve to the same server project.
2. **Launch your agents.** Select the project and use **+ Codex** and **+ Claude**.
   Each gets its own terminal and execution identity. Launch more sessions when
   you have independent work for them.
3. **Enable task delivery.** Open **Settings → Gateway Server**. Fresh installs
   use **Server** mode to host locally. Enable **Let the amux server assign queued
   work and send task notifications to agents in this app**, then **Save & Close**.
   Keep **Give agents amux tools (MCP)** enabled under **Workspaces & Terminals**
   before launching agents.
4. **Build the queue.** Create tasks in the project's **Tasks** view, or discuss
   the work with an agent and have it create scoped tasks. Idle agents pick up
   eligible tasks automatically, one per agent. Tasks requiring a human decision
   move to **User review**. You can watch progress in Tasks or open the agent's
   terminal and talk directly.
5. **Review the result.** On the server, **Gateway Server → Task automation**
   controls automatic code and documentation reviews for all projects. When
   enabled, completion starts that review sequence, with correction tasks joining
   the same workflow.

For example, launch Codex and Claude for a repository on your Linux workstation,
then do the same on a Windows test machine. Queue independent implementation,
testing and documentation tasks so the agents can work concurrently. Mark Windows
verification with the `os:windows` requirement and Linux verification with
`os:linux`; an unrestricted task can go to either machine. Add a macOS client for
Mac-specific checks. Cross-platform testing runs on the machines where you launch
agents; keep the repository available on each.

### Connect another machine

Choose one machine as the server. In its **Settings → Gateway Server**, enable
**Allow connections from other machines**, set a server name or IP address the
other machines can reach, and save. Reopen Settings and copy the **Client endpoint**
and a temporary **peering key** (generate one if needed).

On each other machine, choose **Client** mode, enter the server address and peering
key, and click **Connect**. Enable task delivery there too and save. Open or clone
the same repository and launch Codex and Claude on that machine. Each client runs
its terminals locally while sharing the server's task queue. Peering keys expire
within ten minutes, so generate another if needed.

See [project agents and OS routing](docs/gateway-cli.md#project-agents-and-os-routing)
for CLI discovery, handoffs and OS requirements, and
[queued work assignment](docs/gateway-delivery.md#queued-work-assignment) for
eligibility and delivery behavior.

## Tools and memory for every agent

With **Give agents amux tools (MCP)** enabled (the default), new desktop Codex and
Claude sessions receive an `amux mcp` stdio server. It exposes task management and
[work submission](docs/gateway-cli.md#submitting-work), memory search/get/store/update,
and targeted code and documentation exploration through the bundled tools.
Agents can retrieve a symbol or a document section without loading entire files.
Launch-time settings preapprove amux tools for that session; provider restrictions
and configured developer instructions still apply.

Relevant project memories accompany session guidance and task assignments. Idle
memory review prompts agents to search existing memories and update or store
durable decisions, corrections and discoveries. This helps later sessions reuse
what the team learned. Sessions receive guidance on startup, resume and context
restoration; inspect the templates in **Settings → Instruction Templates**.
Memory sharing uses agent-memory's synchronization setup;
pairing desktops alone does not configure that tool. See
[agent instructions](docs/agent-instructions.md) for recall and review behavior.

## Everyday controls

Open **Settings & Gateway** for **Gateway Server**, **Workspaces & Terminals**,
**Agent Instructions**, **Instruction Templates**, and **Updates**.

Select a local or remote project to manage its task queue, even without opening
a checkout. Tasks and subtasks retain their parent context in the desktop and CLI.
Tasks search accepts full or shortened task IDs, title words and tags across all
states. **Clear** restores the selected Active, User review or Completed view.

Pin favorite projects at the top of the sidebar; favorites persist on this device.
The project filter ignores case, showing exact matches first and substring matches
when there is no exact match. Unadded local checkouts have an explicit **Add**
action; remote projects can be cloned from their context menu.

Each terminal tab has its own **×** close button and directory subtitle. Closing a
workspace leaves project files on disk; running terminals require confirmation.
Saved layouts reopen terminals, with native agent continuation controlled by
**Resume agent sessions on startup**. Under **Workspaces & Terminals**, you can
also opt in to **Start amux when I sign in**. **Clear context before new tasks**
is a separate, optional setting.

**New workspace → Script** starts an interactive initialization script; General
also accepts an optional script. Unix uses POSIX shell; Windows supports `.ps1`,
`.cmd` and `.bat`. Scripts run on explicit terminal launches, including new tabs
and splits, and are not rerun by layout restoration.

The update bell opens **Settings → Updates**. Supported direct-download installs
offer **Update now**, followed by an explicit restart; package-managed installs
show their upgrade instructions. Updates replace the portable bundle executables
or the whole macOS app. See [app updates](docs/updates.md).

Submit changed files with `amux submit-work` or MCP `submit_work`.
**Workspaces & Terminals → Work submission** selects commit-and-push or
pull-request mode. Use `amux tasks report <from-ref>..<to-ref>` for a read-only
report of commits, linked tasks and reviews, and exact-commit CI status. See
[work reports](docs/gateway-cli.md#work-reports) for date ranges and partial-data
behavior.

## Build and use the CLI

With Rust 1.96.1 installed (the CI toolchain):

```sh
cargo build --bins                                   # amux (CLI) and amux-desktop (GUI)
target/debug/amux-desktop --project-dir /path/to/project
cargo run -- agent codex --mcp
cargo run -- whoami
cargo test --no-default-features --tests
```

MCP code and memory tools require the bundled `agent-tools` and `memory`
executables alongside amux; release bundles include them. For source builds, see
the pinned targets in `packaging/bundled-tools.json` and
`scripts/fetch_bundled_tools.py`.

`amux --project-dir /path/to/project desktop` opens the desktop from a console.
`amux whoami` prints the calling agent's project and execution identity;
`amux agents` lists live local agents, including those launched from shell tabs.
Outside the desktop, `amux agent <provider> --mcp` launches an interactive agent
with the same MCP tools.

## Documentation

- [Login startup](docs/login-startup.md): opt-in startup on Linux, macOS, and Windows.
- [StateMachine](docs/StateMachine.md): durable events, controller plugins,
  reconciliation outcomes, and the client observation/delivery boundary.
- [Server architecture](docs/ServerArchitecture.md): native hosting, client/server
  topology, security, native messaging and storage.
- [Architecture background](docs/architecture.md): original handoff design and
  owner decisions, superseded where the server and StateMachine docs specify.
- [Features](docs/features.md): CMUX parity baseline, agent capabilities, scope,
  and acceptance scenarios.
- [Tooling](docs/tooling.md): language, terminal engine, and UI candidates, and
  how they are evaluated.
- [MVP plan](docs/mvp.md): milestones, closing criteria, reviewer evidence, and
  remaining gates.
- [Gateway CLI](docs/gateway-cli.md): task and inline message commands.
- [Native backend hosting](docs/server/Compatibility.md): run `amux server secure`
  with enrolled mTLS clients and optional external channel integrations.
- [Agent instructions](docs/agent-instructions.md): detect providers, install and
  verify managed task guidance, and configure local task tools from Settings.
- [Gateway delivery](docs/gateway-delivery.md): automatic notifications, session identity
  and built-in terminal agent identity.
- [Reminder validation](docs/reminder-validation.md): historical selected-task
  probes and the current event-driven native delivery checks.
- [Native review](docs/windows-review.md): bundle checks for Linux and Windows
  reviewers.
