# Parity status

Revision: amux `fc5bf4c` on `mvp/native-terminal`, October 22, 2026.

This maps every row of the parity baseline in [features.md](features.md) to the
current amux source. It records what exists, not acceptance. The pinned CMUX
baseline stays the target. Nothing here claims parity, and every original
requirement stays open until proven. Milestone criteria are in [mvp.md](mvp.md).

## Status terms

- **Missing:** no amux implementation.
- **Partial:** some of the row is implemented; the gap is named.
- **Implemented:** the row's behavior exists in source. This alone is not acceptance.

Acceptance columns record runtime evidence on amux itself, never CMUX's evidence:

- **None:** no runtime evidence.
- **Local:** tests or a run by the amux implementer only.
- **Review:** a CMUX platform reviewer ran an amux artifact. The finding is noted.

Linux review so far found changes required (L1–L4). Windows review of the `57d07a0`
bundle covered the CLI, provider launch and a responding window only; interactive
checks did not run.

## Capability map

| # | Baseline capability | amux source | amux status | Linux acceptance | Windows acceptance |
| --- | --- | --- | --- | --- | --- |
| 1 | Terminals, panes, tabs, splits, selection, clipboard, resize | Real PTYs via `vendor/iced_term` (Alacritty engine); tabs, splits, focus, resize in `src/desktop.rs`; selection and standard-clipboard copy/paste in `vendor/iced_term/src/view.rs`, `bindings.rs` | Implemented | Review (changes required) | Review (window and launch only) |
| 2 | Copy-on-select, PRIMARY paste, workspace order, colors, identity/focus shortcuts | Standard clipboard only | Missing | None | None |
| 3 | Tab reorder, cross-pane/workspace moves, pointer drops, directional split | Splits only (`Message::Split`) | Missing | None | None |
| 4 | Persistent workspace groups | Flat workspace list (`Message::NewWorkspace`, `SelectWorkspace`) | Missing | None | None |
| 5 | Project configuration and actions (`cmux.json`, palette, layouts, restart policies) | None | Missing | None | None |
| 6 | Manifest-aware project inspector | None | Missing | None | None |
| 7 | Files tree, Markdown preview, Git graph and diffs | None | Missing | None | None |
| 8 | Agent diff surfaces | None | Missing | None | None |
| 9 | Durable repository review comments | None | Missing | None | None |
| 10 | Sidebar branch/PR, status, notes, Git polling | Sidebar shows gateway tasks only (`task_list` in `src/desktop.rs`) | Missing | None | None |
| 11 | Per-surface notification inbox (OSC 9/99/777, CLI notify, unread) | None | Missing | None | None |
| 12 | Desktop notification actions, bells, workspace attention | Opt-in native process-exit notifications (`src/notifications.rs`, `src/desktop.rs`), bounded asynchronous delivery and separate outcome footer | Partial: no notification actions, bells or workspace attention | Local (visible private-display popup and missing-service check) | None (native module type-checked; popup review pending) |
| 13 | Agent lifecycle/status notifications, native resume IDs, provider hooks | Process exit routed to the exact tab with an optional OS notification; no richer lifecycle status, resume IDs or hooks. Hooks are not used for reminders (D6) | Partial: process exit only | Local (native GUI process exit) | None |
| 14 | Fixed Codex/Claude launch commands, per-tab Resume last | Provider tabs and `amux agent codex\|claude` with per-launch identity (`src/main.rs`, `src/identity.rs`) | Partial: no Resume last | Local; Review (launch) | Review (CLI launch only) |
| 15 | Reviewed exact/prefix resume approval | None | Missing | None | None |
| 16 | Session geometry, order, topology, selection, history replay | Workspaces, split topology, tabs and active workspace saved atomically (`src/session.rs`); restored processes stay stopped | Partial: no window geometry, no scrollback replay, no browser locations | Local (`tests/session_contract.rs`, GUI restore smoke) | None |
| 17 | Last cwd, launch-environment persistence, previous-session restore, unclean-launch backup | Workspace directory saved; corrupt or unknown snapshots preserved, not overwritten | Partial: no live cwd tracking, no environment persistence | Local (`tests/session_contract.rs`) | None |
| 18 | CLI/control API, socket discovery, caller identity, peer auth | CLI: `agent`, `whoami`, `agents`, `tasks`, `messages` (`src/main.rs`); caller identity by registered process ancestry (`src/identity.rs`) | Partial: no socket or named-pipe control API for the desktop | Local (`tests/caller_contract.rs`, `launcher_contract.rs`, `gateway_contract.rs`, `identity_contract.rs`, `messages_contract.rs`) | None |
| 19 | Gateway routing, recipient/process-generation fences, input protection, injection consent | Explicit task selection with per-agent consent, exact recipient binding, readiness, checked submission and durable dedup (`src/recipient.rs`, `readiness.rs`, `submission.rs`, `delivery.rs`) | Partial: Linux with Codex 0.163 only; no automatic routing or session-start recovery; non-Linux input evidence returns none, so delivery defers | Local (opt-in real Codex turn; see [reminder-validation.md](reminder-validation.md)) | None |
| 20 | SSH workspaces | None | Missing | None | None |
| 21 | Mosh and Mosh-tmux lane | None | Missing | None | None |
| 22 | Listening-port attribution and forwarding | None | Missing | None | None |
| 23 | Browser surfaces and automation | None | Missing | None | None |
| 24 | Remote browser routing | None | Missing | None | None |
| 25 | Named persistent browser profiles | None | Missing | None | None |
| 26 | Native Claude teams pane translation | None | Missing | None | None |
| 27 | Diagnostics, request correlation, resource snapshots, benchmarks | None beyond test evidence files | Missing | None | None |
| 28 | Platform paths, private storage, atomic replacement, process identity, native input | Per-user state dir (`directories`), `0600` agent registration, atomic layout save, process generation on Linux and Windows, Linux terminal-session guard (`src/terminal_lifetime.rs`) and Windows Job Object guard (`src/terminal_lifetime_windows.rs`) | Partial: Windows guard starts after enrollment; late forks, abrupt death and reaping remain acceptance gaps; no macOS process identity or native non-Linux input evidence | Local; Review (L3 cleanup partly fixed) | Local (native ConPTY cleanup tests); interactive review pending |
| 29 | Updates and distribution | CI review bundles with separate CLI and GUI entry binaries, both hashes in `SHA256SUMS` and `BUILD.json`; immutable manual review releases | Partial: no packages, installer or updater | Local (native build, bundle verification and GUI launch) | Local (native build and bundle verification); Review (earlier launch artifact) |

Totals: 1 Implemented, 9 Partial, 19 Missing.

## Evidence

Reproduce the local evidence from the repository root:

```sh
cargo test --locked --no-default-features --tests   # core identity, gateway, session tests
cargo test --locked --bin amux --test terminal_contract --test recipient_contract --test terminal_lifecycle_contract
cargo test --locked --test notifications_contract --test native_notification_contract
```

Opt-in provider checks are in [reminder-validation.md](reminder-validation.md).
Review bundle checks are in [windows-review.md](windows-review.md). The Linux and
Windows review findings are on CMUX tasks `01a164c9-f932` and `01a164c9-fac2`.
The Linux follow-up is CMUX task `01a165a5-eba1`. These reviews remain separate
from implementer evidence. Notification behavior and limits are recorded in
[notification-validation.md](notification-validation.md).

## macOS feasibility

macOS is a new target (features.md) and an M3 feasibility gate. It is not
shipped, built or tested.

**Dependencies support macOS upstream.** [Iced](https://github.com/iced-rs/iced)
lists macOS as a supported platform and renders through
[wgpu's Metal backend](https://wgpu.rs/doc/wgpu/documentation/features/index.html).
The terminal engine is [Alacritty](https://github.com/alacritty/alacritty)'s,
which ships on macOS; the vendored widget already has macOS key bindings
(`vendor/iced_term/src/bindings.rs`). `reqwest` with rustls,
[`sysinfo`](https://github.com/GuillaumeGomez/sysinfo), `directories` and
`signal-hook` all support macOS.

**amux itself is not ready for macOS:**

- **Process generation:** `process_generation` returns an error on macOS
  (`src/identity.rs`), so every task mutation and message send fails there.
- **Agent teardown:** the launcher's parent-death fence uses Linux `prctl`
  (`src/main.rs`); terminal cleanup has Linux session and Windows Job Object
  guards. macOS has neither native implementation.
- **Input readiness:** native input evidence returns none outside Linux
  (`src/recipient.rs`), and the readiness profile covers Linux with Codex only.
  Reminders would always defer.
- **Build and runtime:** there is no macOS CI job, no native build, and no runtime
  acceptance. Packaging, code signing and notarization are not started.

Before any macOS claim, these need a native macOS implementation and the same
reviewer evidence that [mvp.md](mvp.md) requires for Linux and Windows.
