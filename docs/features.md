# amux features

This document lists what amux must do. How the pieces fit together is in
[architecture.md](architecture.md); language, terminal, and UI choices are in
[tooling.md](tooling.md). Owner decisions were recorded on October 18, 2026, on
gateway task `01a14fd1-97e2-7583-a766-e4e1d8f18933`.

## Product direction

amux is a full rebuild of CMUX (`cmux-linux` / cmux-gtk) in this repository.
It targets Linux, Windows, and macOS. It keeps the useful native capabilities
of today's CMUX and makes agent collaboration on one machine and across
machines first-class.

## Parity baseline

The baseline is CMUX commit
[`dc9bc256d9b680170124edaaed5e1d3702c4a066`][cmux]. This is an inventory of
existing behavior and its evidence, not a claim that amux implements it.

| Platform | CMUX today | amux target |
| --- | --- | --- |
| Linux | Supported desktop | Preserve existing behavior |
| Windows | Experimental native preview | Preserve preview behavior, including documented absences |
| macOS | No implementation | New target with its own acceptance scope (open decision) |

The checklist below expands the [CMUX delivery matrix][matrix] and
[component boundaries][components]. Linux status means:

- **Verified:** the pinned matrix records prior verification, not a new amux result.
- **Pending:** implemented, but cumulative Actions or specific runtime acceptance
  remains pending. Earlier passing checkpoints do not close that larger row.
- **Implemented:** present in the component inventory; that alone establishes no
  complete acceptance result.

Windows **Shared preview** means the shared application contains the capability,
subject to native shell/command/service compatibility. It does not mean separate
Windows acceptance is complete. **Unestablished** means the pinned evidence does
not establish that workflow on Windows. The [Windows preview notes][windows]
remain authoritative for absences and required destination/hardware validation;
Linux verification must not be promoted to Windows verification.

| Capability to preserve | Linux baseline / acceptance | Windows baseline / availability | Pinned CMUX evidence |
| --- | --- | --- | --- |
| Native terminals, panes, sibling tabs, splits, selection, clipboard and resize | Implemented | Shared preview; native ConPTY | [Components][components], [Windows][windows] |
| Copy-on-select, PRIMARY middle-click paste, generic copy/paste; workspace order, colors and identity/focus shortcuts | Verified | Shared clipboard/order controls; X11 PRIMARY behavior is not established | [Matrix][matrix] |
| Live tab reorder, cross-pane/workspace surface moves, pointer drops and directional split; retain UUID, process, focus and saved topology | Pending; Linux adaptation. Reject moving the final SSH terminal; browser routes rebind on transfer | Shared preview; pointer/native acceptance not established | [Surface topology][moves] |
| Persistent workspace groups: order, membership, color, collapse, unread aggregation; keyboard/CLI/UI controls | Pending | Shared preview | [Groups][groups] |
| Project configuration/actions: cmux.json resolution, reviewed palette, named commands, custom layouts, launch environment and restart policies | Pending | Shared preview where commands are compatible; workspace startup scripts unavailable | [Project actions][projects], [Windows][windows] |
| Manifest-aware project inspection: Files, Targets, Settings, Actions; Rust, Go, Node, Python, CMake and Make discovery | Pending; Linux adaptation of Xcode inspector | Unestablished | [Project inspector][inspector] |
| Files tree/Markdown previews; Git commit graph and per-file diffs with bounded/cancellable reads | Implemented | Shared preview | [Components][components] |
| Agent diff surfaces from patch file/stdin, unstaged, staged, merge-base branch and last-turn snapshots; retained browser controls/focus | Pending; last-turn capture best effort and bounded | Shared code; native browser/Git workflow acceptance unestablished | [Diff surfaces][diffs] |
| Durable repository review comments: UUIDs, line ranges, compose/list/add/delete/consume, retained consumed history | Pending; independent CLI and line-anchored viewer | Shared code; end-to-end acceptance unestablished | [Diff surfaces][diffs] |
| Sidebar branch/PR/MR, status/progress, links, multiline Markdown, description/latest notification; automatic Git polling/provenance | Pending | Shared preview | [Metadata][metadata], [Git provenance][git-metadata] |
| Per-surface notification inbox: OSC 9/99/777 and chunk completion, CLI/socket notify, read/clear/dismiss, jump to unread, focused suppression, exact click target and bounded history | Verified through core cumulative checkpoint | In-app inbox and attention available; native desktop toasts unavailable | [Matrix][matrix], [Inbox][inbox], [Windows][windows] |
| Desktop notification actions, terminal bells, workspace attention and rate/bounds behavior | Core desktop delivery previously verified; retain bounded admission | Inbox/attention remain; native notification/bell adapter absent | [Desktop delivery][desktop], [Components][components], [Windows][windows] |
| Agent lifecycle/status/turn notifications, native resume IDs and stale binding protection; provider setup and lifecycle traces | Pending; all 18 catalog hook integrations plus Claude wrapper implemented | Shared integrations need compatible provider commands; live provider acceptance pending | [Matrix][matrix], [Provider checkpoints][providers] |
| Fixed Codex/Claude launch commands and per-tab Resume last setting | Implemented | Shared preview with native shell/provider setup | [Components][components] |
| Reviewed exact/prefix resume approval bound to command, cwd and environment; manual/automatic controls and revocation | Pending at detailed policy checkpoints; do not infer complete trusted-provider/per-agent controls | Shared preview; approval/runtime evidence not independently established | [Exact approval][exact], [Prefix approval][prefix] |
| Session geometry, workspace order, topology, active selection, browser locations; final durable save and bounded terminal history replay, including unopened background surfaces | Core session/window restoration previously verified; detailed replay acceptance remains pending | Shared preview; destination validation remains necessary | [Matrix][matrix], [History replay][history] |
| Last reported cwd, explicit launch-environment persistence, previous-session restore and unclean-launch backup preservation | Implemented; detailed runtime/cumulative acceptance pending | Shared preview; arbitrary process recovery not provided | [Directories][cwd], [Environment][env], [Previous session][previous], [Crash recovery][crash] |
| CLI/control API, socket discovery, caller identity, peer authentication, correlated bounded responses and completions | Implemented | Native owner-secured named pipe; CLI independent of GUI initialization | [Components][components], [Windows][windows] |
| Gateway fresh-message routing, exact recipient/process-generation fences, prompt/input protection and explicit injection consent | Implemented; live acceptance separate | Native process creation-time/ConPTY fences; live destination acceptance separate | [Components][components], [Consent UI][consent], [Windows][windows] |
| SSH workspaces: remote daemon, reconnect, reverse CLI relay, split/tab inheritance, management and automatic forwarding | Pending | Shared code; native SSH/remote deployment acceptance unestablished | [Matrix][matrix], [Components][components] |
| Mosh and Mosh-tmux terminal lane with SSH management/fallback; signed remote resume and EOF retain-until-next-input policy | Pending; full UDP integration environment-dependent | Unestablished; local client/POSIX command compatibility required | [Mosh/resume][mosh] |
| Listening-port attribution to terminal descendants, exit retirement, local/remote provenance, sidebar/API and forwarding | Pending | Automatic local TCP attribution unavailable; remote workflow acceptance unestablished | [Matrix][matrix], [Windows][windows] |
| Independent browser surfaces, nearest-right placement, local files/history, snapshots/evaluation/click/screenshots and navigation | Implemented; cumulative browser acceptance pending | External browser service requires compatible native installation; acceptance unestablished | [Matrix][matrix], [Components][components] |
| Remote browser routing through selected workspace, subresources, SOCKS, same-port isolation and reconnect | Pending | Unestablished | [Matrix][matrix] |
| Named persistent browser profiles, per-surface identity/restoration; extensions and password-manager workflows | Profile reuse implemented; cumulative profile and extension/password-manager acceptance not established | Compatible external browser service required; acceptance unestablished | [Profiles][profiles] |
| Native Claude teams pane translation: teammate splits/layout/resize/respawn/close with stable identity, metadata and notifications | Pending; real-provider acceptance remains open | POSIX tmux compatibility shim unavailable; ordinary native sessions remain | [Teams][teams], [Windows][windows] |
| Bounded diagnostics, request correlation, resource snapshots, diagnostic collection and optimized benchmarks | Implemented; benchmark tools are not proof of measured amux performance | Preview diagnostic logs/build and smoke artifacts available; hardware validation still required | [Components][components], [Windows][windows] |
| Platform paths/private storage, atomic replacement, process/peer identity, installation ownership and native appearance/input | Implemented through platform boundary | Native Windows paths/security/process and GTK/WGL services; unsupported services report absence | [Components][components], [Windows][windows] |
| Updates and distribution: DEB, RPM, archive, icons/launchers/completions/man page, Homebrew Cask; package-manager installs never self-replace | Implemented distribution/update policy; preserve verification requirements | Experimental portable ZIP/checksum, explicit staged update, user-data preservation and recoverable replacement; no installer or unattended restart | [Build/distribution policy][distribution], [Windows][windows] |

Provider lifecycle integrations are existing mechanisms for status, resume and
notifications, not a requirement to deliver reminders through hooks. D6 requires
amux's session-start/continuation reminders to use direct terminal injection
without provider or agent-tools hooks. Retain explicit user consent and recipient,
readiness and human-input protections; optional hook compatibility must not become
a reminder prerequisite.

### Explicit adaptations and exclusions

- CMUX restores one native window. Multiple native windows are a separate product
  change, not existing parity. History replay and reviewed agent resume do not
  checkpoint arbitrary running processes or add tmux hibernation.
- Browser profiles do not mean importing another browser's cookies or credentials.
  Remote image drag-and-upload is also outside the pinned desktop milestone.
- The Windows preview lacks native toasts, automatic TCP listener attribution,
  workspace startup scripts and the Claude Teams POSIX shim. Preserving the preview
  includes these documented absences; filling them is not newly approved scope.
- iOS UI/Apple host plumbing, mobile-host compatibility, Iroh adoption and cmux-tui
  protocol compatibility are not delivered desktop capabilities. Their separate
  authentication/protocol boundaries remain explicit in [platform applicability][applicability].

These are the pinned [scope boundary][scope], not claims of completed upstream
equivalence. macOS acceptance remains open. Terminal behavior and native input
evidence are covered in [tooling.md](tooling.md); ABI names and existing implementation
languages are not product requirements.

The browser must keep its service boundary. A toolkit WebView does not
establish parity for persistent profiles, extensions, password managers,
automation, or remote routing.

## Works without a gateway

Terminals, agents, sessions, and local desktop notifications work with no
gateway connection. Task features require a gateway, which may run on the same
machine. The desktop application stays upgradable and restartable at any time
without losing gateway-accepted task state. Persistence of requests not yet
accepted by the gateway remains undecided; this implies no second local task
store, durable outbox, or mandatory background process.

## First-class agent capabilities

amux exposes gateway features without a separate agent-tools installation. The
desktop and the shipped agent-facing interface use the same underlying
operations.

- **Tasks:** creation, discovery, claims, assignment, delegation, comments,
  completion, cancellation, and handoff context.
- **Identity and routing:** machine credential, agent identity, platform
  eligibility, and routing status, including pending or uncertain delivery.
  Every running Claude/Codex session has a distinct identity, including three
  agents in one project/provider. Tasks can be delegated to an exact session;
  another session or restarted process cannot inherit its claim implicitly.
- **Scratch text and documents:** publication, discovery, and retrieval in task
  handoffs, with provenance preserved. Text is passed inline in tasks and
  messages. Work unrelated to a repository is never committed to Git to share
  it.
- **Reminders:** direct terminal injection of pending gateway reminders on
  session start or continuation, and of fresh task notifications, with no
  dependency on provider or agent-tools hooks.
- **Desktop notifications:** local only. External notifiers such as Discord
  belong to the gateway.

The UI shows requests awaiting submission separately from requests the gateway
has accepted. A shared claim succeeds only when the gateway acknowledges it. A
reminder for previously claimed work shows who claimed it, from which machine,
and that the outcome is unknown.

## Out of scope

- Memory. Agents use their native memory or agent-memory. amux does not store,
  sync, or expose memory.
- agent-tools code exploration, indexing, and knowledge graph, unless decided
  separately.
- Automatic re-execution of work after a crash or reboot.
- A plugin framework, dynamic library loading, or marketplace for notifiers.

## Acceptance scenarios

Production cross-machine execution acceptance requires gateway handoff evidence.
The [proposed evaluation plan](tooling.md#evaluation-gates) permits independent
terminal/UI feasibility work in parallel. These are future acceptance cases,
not reports of successful tests:

1. Linux delegates to Windows while Windows is off; Windows reconnects and
   receives a reminder.
2. Crash after a claim, or after an explicit start report: recovery shows the
   last known claimant/machine, any reported start and an unknown outcome requiring
   progress checks. No automatic rerun occurs; this does not select a persisted
   `needs-intervention` enum.
3. Retrying the same request ID creates no duplicate task.
4. Destination amux fetches a task and submits safe terminal input, but the agent
   makes no claim or start report. Record destination receipt and input submission
   only; agent understanding and execution remain unknown. A later claim confirms
   ownership, not start, until the executor explicitly reports it.
5. Duplicate events or retries of one delivery to the same recipient generation
   submit at most one reminder. Reconnect never retargets old input to a new
   process. An uncertain paste/Enter attempt retains its fence and is not blindly
   retried; arbitrary work side effects are not promised exactly once.
6. With partial human input, busy state or uncertain readiness, delivery waits
   without overwriting or interrupting input. Before any later submission, recheck
   recipient/process generation, consent and readiness. Withdrawing consent
   retires pending terminal deliveries without deleting accepted tasks.
7. After reconnect, a task accepted more than five minutes earlier is recovered
   through an authoritative query and a fresh session-start/continuation reminder;
   old transient terminal events are not replayed. Gateway unavailability leaves
   terminals usable and unsent requests visibly separate from accepted tasks.

The [delivery contract](architecture.md#delivery-contract) defines recorders and
evidence for each stage. A terminal write, Enter or notification success alone
proves neither agent acknowledgment nor execution. Persistence of requests still
awaiting acceptance remains undecided.

Also required:

- Gateway operations work with agent-tools absent.
- Notifier failures are isolated from task acceptance, execution, and other
  notifiers.
- Pending-task reminder delivery works with provider hooks unconfigured or
  disabled and agent-tools absent, including session continuation, busy or
  partially typed input, reconnect, and duplicate suppression.

Verification follows each repository's GitHub Actions policy.

[cmux]: https://github.com/nitecon/cmux-gtk/tree/dc9bc256d9b680170124edaaed5e1d3702c4a066
[components]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Components.md
[windows]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/WindowsPreview.md
[consent]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/src/gateway/ui.rs#L1-L36
[distribution]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Architecture.md#build-and-verification
[applicability]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/PlatformApplicability.md
[matrix]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#delivery-and-evidence-matrix
[moves]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#surface-move-and-drag-topology-checkpoint
[groups]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#persistent-workspace-groups
[projects]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#project-configuration-resolution-foundation
[inspector]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#manifest-aware-linux-project-inspector-checkpoint
[diffs]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#agent-accessible-diff-surface-checkpoint
[metadata]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#multiline-metadata-block-checkpoint
[git-metadata]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#git-tracking-and-provenance-checkpoint
[inbox]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#notification-inbox-protocol-checkpoint
[desktop]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#desktop-message-delivery-checkpoint
[providers]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#codex-lifecycle-hooks
[exact]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#exact-command-approval-checkpoint
[prefix]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#literal-prefix-approval-checkpoint
[history]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#session-scrollback-replay-checkpoint
[cwd]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#restored-working-directory-checkpoint
[env]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#workspace-launch-environment-persistence
[previous]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#previous-session-startup-recovery-checkpoint
[crash]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#unclean-launch-recovery-checkpoint
[mosh]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#mosh-interactive-transport
[profiles]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#browser-profile-reuse
[teams]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#native-claude-code-teams
[scope]: https://github.com/nitecon/cmux-gtk/blob/dc9bc256d9b680170124edaaed5e1d3702c4a066/Docs/Parity.md#current-main-scope-boundary
