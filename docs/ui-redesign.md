# Native workspace design

The visual reference is [the owner-provided TSX prototype](amux_ui_redesign_prototype.tsx).
The desktop remains a native Rust/Iced application with native terminal panes.

## Product decisions

- Settings uses the reference's in-window modal.
- Repository matching is required for task delivery.
- Activity means native terminal output within 30 seconds. Input alone does not
  activate a session; output echoed by the terminal still counts. It does not
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
The UI uses the fresh native-notification preference; legacy manual-reminder
controls that enter screen-based readiness are not exposed.
Single view maximizes a pane; Restore panes brings its peers back without
terminating their sessions. Tabs retain close controls and directory subtitles.

Tasks default to Active (queued and in-progress), with User review and Completed
as the other filters and a pinned composer. Parked review tasks show their details
and comments, with human actions to return them to the queue or close them.
Agents request review through `amux tasks user-review` using their assigned
identity; parked tasks are excluded from agent reminders, pickup and injection.
The task panel can collapse to a narrow arrow rail
or detach into a larger window. Docking or closing that window restores the panel
without closing sessions or losing drafts. Gateway
acceptance and uncertain submission receipts remain visible, with no automatic
POST retries or fabricated ownership. Lifecycle events refresh matching task
lists; concurrent refreshes coalesce. The left-side Gateway messages drawer shows sender bubbles with title, 200-character
preview, task state at the event, provenance, task ID and age. It retains the latest
50 previews across restart and searches messages, receipts and connection information. Clear hides entries from that view and never deletes
or acknowledges delivery records.

Open a task to manage it in the right-hand column. The detail view displays its
description, specification and comment thread, with Edit, Assign and Close task
actions. Edit changes the title, description and specification; Close task marks
it completed. Comments are posted as the user. Assignment lists the gateway's
available local and gateway-connected agents with machine, provider and OS labels
and targets the selected execution. Each active task card also has a direct Assign
action. Opening assignment refreshes local presence across matching workspaces
before reading the gateway directory.
The gateway retains normal inactivity-based unassignment. Pending actions block
duplicate submission, and unconfirmed results require a refresh before repeating
the action; drafts are retained. Switching workspaces never retargets a pending
operation.

Files browses actual workspace directories and previews text. Git shows the
actual branch, working tree and recent commits. Both are read-only. Settings
persists process-exit notifications and startup agent resume alongside the
existing gateway preferences. General changes do not rebind the gateway or
discard an uncertain task request. The update bell uses the existing verified
binary installer and restart flow.

## Task routing

The composer supports automatic project routing, queue unassigned, broadcast,
and an exact live agent execution. The gateway recipient directory is scoped to
the workspace's canonical Git remote. Ordinary agent presence uses the existing
URL/API key and the terminal's assigned native identity; no additional machine
or session keys are required. Availability refreshes every 30 seconds and uses
the gateway's 120-second lease. Closed agents stop advertising and expire there.

Human task creation sends no agent origin headers. The selected session and
generation come from the directory; stale or unavailable selections are never
silently changed to automatic routing. Submission rechecks the actual Git
remote and project, and explicit routing must be acknowledged by the gateway.
Errors retain an uncertain receipt and never automatically replay a POST.

Queue-only creation does not immediately inject a lifecycle notification. With
automatic gateway delivery enabled, an agent idle for 30 seconds without output
and without a draft or owned active task can receive the first eligible queued
item. The same 30-second clock drives the workspace indicators. The app confirms
one claim for the first eligible idle session, then injects a request to evaluate
the task, implement viable work, or discuss/close obsolete work with a reason.
Task ownership and native idleness are checked again before input. Unconfirmed
claims or input are fenced and never automatically repeated after restart.
Broadcast creates one
unclaimed task and fans out to matching local agents; agents still arbitrate
ownership through normal claims. Exact creation targets only the selected
execution. Later comments follow current ownership, and released tasks can route
normally. Existing repository, goal, self-origin and offline-history gates remain.

Gateway support was delegated through task
`01a16f17-6788-7791-b5d2-a052b207c38a` and deployed in v1.23.0. Older gateways
retain automatic routing while the UI explains that explicit recipients are
unavailable. The published v0.0.11 testing release predates this integration.

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
subsystems and bundled Lucide notice checked. The final design commit `c1f7b0c`
also passed both platforms ([CI run](https://github.com/nitecon/amux/actions/runs/40124785852));
both portable bundles were verified against their hashes and source revision.
Native Windows visual review remains an owner testing step.

Recipient integration has local HTTP and durable queue tests for attribution,
repository fences, all delivery modes and stale execution handling. An isolated
native GUI fixture submitted each of the four composer modes exactly once with
matching repository metadata and no agent impersonation headers. Evidence is
`artifacts/design-preview/routing-native/evidence.json`.
