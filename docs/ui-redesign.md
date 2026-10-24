# Native workspace design

The visual reference is [the owner-provided TSX prototype](amux_ui_redesign_prototype.tsx).
The desktop remains a native Rust/Iced application with native terminal panes.

## Product decisions

- Settings uses the reference's in-window modal.
- Repository matching is required for task delivery.
- Activity means native terminal input or output within five seconds. It does not
  imply that a model is reasoning. No rendered terminal text drives this metric.
- Sample transcripts, file locks, model names, quotas and the simulation panel
  from the prototype are illustrative data and are not shipped as live features.
- Workspace creation retains existing-folder, clone and script workflows, with
  an optional saved display name. The home-directory workspace remains Default.

## Implemented surfaces

The header exposes connection state, agent activity, Events, app updates and
Settings. `Ctrl+,` opens Settings without forwarding the shortcut to the terminal
or replacing its typed input. A 256-pixel workspace sidebar and 320-pixel task column surround the
native panes. Narrow windows use compact, labeled-on-hover toolbar controls.
Single view maximizes a pane; Restore panes brings its peers back without
terminating their sessions. Tabs retain close controls and directory subtitles.

Tasks have All, Active and Completed filters and a pinned composer. Gateway
acceptance and uncertain submission receipts remain visible, with no automatic
POST retries or fabricated ownership. Lifecycle events refresh matching task
lists; concurrent refreshes coalesce. The Events drawer searches real receipts
and connection information. Clear hides entries from that view and never deletes
or acknowledges delivery records.

Files browses actual workspace directories and previews text. Git shows the
actual branch, working tree and recent commits. Both are read-only. Settings
persists process-exit notifications and startup agent resume alongside the
existing gateway preferences. General changes do not rebind the gateway or
discard an uncertain task request. The update bell uses the existing verified
binary installer and restart flow.

## Remaining integration

Exact agent, unassigned and broadcast targets in the prototype's task composer
require a gateway API contract. Gateway support is delegated to owning-project
task `01a16f17-6788-7791-b5d2-a052b207c38a`; the amux tracker is
`01a16f17-6788-7791-b5d2-a065f4979044`. Until integrated, the composer honestly
uses automatic project routing. The redesign goal remains open.

## Validation

Local native Linux screenshots were compared with a rendered TSX reference at
1600×1000 and tested at 1100×700. An isolated HTTP fixture verified a single
accepted task POST after a double-click, exact multiline context, and absence of
agent impersonation headers. Named workspace creation and persistence, task
filtering, Files/Git views, Settings and Events were exercised in the native app.
No live gateway mutation or model turn was used for these checks. Evidence is
under ignored `artifacts/design-preview/`, including `native-evidence.json`.

Core, desktop, terminal protocol and notification checks run through the normal
CI workflow on Linux and Windows. The initial review commit `ba0449c` passed both platforms, including native
terminal tests and optimized builds ([CI run](https://github.com/nitecon/amux/actions/runs/40119844767)).
Both artifact sets had their binary hashes, source revision, Windows executable
subsystems and bundled Lucide notice checked. Native Windows visual review and
the eventual release remain pending.
