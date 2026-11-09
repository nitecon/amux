# Changelog

## v0.0.72 - 2026-11-09

### Fixed
- Retry GitHub release download server errors once through the public asset API,
  retaining TLS and checksum verification when the normal redirect path fails.
- Bundle agent-memory v1.13.10 to reject edits to known tombstoned gateway
  memories before changing local content, tags, or history, with explicit
  instructions for recovering retained lessons under a new memory identity.
- Report tombstone-blocked memories during push instead of silently skipping
  them, and protect newer live mappings from stale pulled tombstones.
- Verify the bundled memory update error and retained content through amux MCP.

### Upgrade notes
- Previously saved local-only edits remain retained. Follow the reported
  `memory copy --id <id> --to <project>` recovery instructions to create an
  editable copy; retrying push on the deleted identity cannot restore it.

## v0.0.70 - 2026-11-08

### Fixed
- Exclude code and documentation review tasks from the work report’s
  “Completed, not released” section.
- Support older GitHub CLI versions when checking work report CI status.

### Changed
- Ignore local agent configuration added by users.
- Update documentation to match current task review reporting, embedded server
  ownership, connection settings, and terminal restore behavior.

## v0.0.69 - 2026-11-08

### Added
- Opt-in desktop startup at sign-in on Linux, Windows, and macOS, configurable
  in Settings > Workspaces & Terminals.
- A server-wide automatic review switch in Gateway settings and one documentation
  review after each completed code review.

### Changed
- Preapprove amux MCP tools for newly launched agent sessions when MCP is enabled,
  while preserving provider deny rules and managed restrictions.
- Shorten injected gateway notices, centralize their templates, and include the
  notified task ID in task and update commands.

### Fixed
- Prevent unchanged explicitly released tasks from being automatically reassigned
  to the same execution, while allowing explicit claims and pickup after relevant
  task changes.
- Bound individual task comments and legacy comment previews so task reads stay
  within the client response limit.
- Keep successfully posted comments acknowledged when returning a task to the
  queue fails, avoiding duplicate posts on retry.
- Fetch bundled runtime tools after Rust cache restoration so native CI tests
  and release bundles retain their required executables.

## v0.0.68 - 2026-11-07

### Fixed
- Load only the newest ten comments in the task viewer and show the total count,
  so long threads exceeding 4 MiB remain available for assignment and closure.
- Read task metadata without comments when claiming, reassigning, changing OS
  requirements, or moving tasks into and out of User review.

## v0.0.66 - 2026-11-06

### Fixed
- Keep review queues moving with best-effort provider and independence
  preferences among available agents in the required project, OS and goal.
  Busy or unavailable reviewers and past comment authors no longer create
  permanent assignment barriers.
- Reset context before a fallback review: send `/clear`, wait five seconds,
  and recheck the process and user input before injecting the review. This
  applies even when optional clearing for ordinary tasks is disabled.
- Compare submit-work paths in project coordinates for repository subdirectories,
  retaining leading whitespace in directory names and PR upstream-edit checks.

### Added
- Resume publication of a matching existing task commit with CLI
  `--resume-commit` or MCP `resume_commit`, preserving unrelated drafts and
  retained stashes without creating another commit.

### Upgrade notes
- Update and restart both the server and desktops. Older desktops cannot receive
  fallback reviews that require the new context-clear delivery capability.

## v0.0.65 - 2026-11-05

### Added
- Display task hierarchies in the desktop and CLI, retaining parent context
  when filters hide it. Agent-created tasks and cross-project handoffs now
  require a parent task; update clients and servers together for this feature.
- Accept large exact file lists through `amux submit-work --paths-file`.
  MCP transfers its existing paths array through a temporary file automatically.

### Changed
- Run server reconciliation every 30 seconds, with presence evaluated before
  queued assignment so agents that remain idle can receive newly queued work.

### Fixed
- Leave delegated destination work available to agents in the target project,
  and clear ownership when releasing a reserved queued task.
- Avoid Windows command-line limits in work submission by using file-based
  path lists for MCP handoff and Git staging and exact-file commits. Large
  submissions retain one commit and preserve unrelated staged and local edits.

## v0.0.64 - 2026-11-05

### Added
- Pin project favorites above the project list and retain them on this device.
  Added local projects sort A-Z; remote projects with agents appear before
  those without agents. Filter by exact name first, with a case-insensitive
  substring fallback when no exact name matches.
- Opt in to **Clear context before new tasks** in Settings. An idle agent
  receives `/clear`, waits five seconds, then receives a first-time task
  assignment after its execution is verified again. Reviews, comments and
  continued work retain their context. This setting is disabled by default.

### Changed
- Show recently updated tasks first, sorting titles A-Z when updates fall
  within the same second.

### Fixed
- Keep agents discoverable when an update renames their running amux wrapper,
  preventing live agents from disappearing from presence and queued assignment.
- Reject interrupted and in-progress clones during checkout reuse while
  preserving partial files, completed empty clones and local edits. Unmarked
  checkouts without a valid commit report an incomplete-checkout error.
- Allow a fresh clone after the user removes a failed clone folder, even when
  its hidden reservation remains.

## v0.0.63 - 2026-11-04

### Changed
- Use a full task queue and detail view for both local and remote projects.
  Select a gateway project to manage and dispatch tasks without checking out
  its repository. Terminal and split controls remain in the bottom toolbar;
  the floating terminal task drawer is removed.
- Open gateway tasks from an unadded local checkout by clicking its project
  name, while keeping the explicit Add action for opening a local workspace.

### Fixed
- Explain when a changed or unavailable remote project binding blocks task
  actions, preserve drafts, and prompt users to reselect the project.
- Keep local task queries, detail updates, routing and mutation receipts from
  becoming stuck after a delegation reset, while retaining connection and
  remote-project reply fences.

## v0.0.62 - 2026-11-04

### Fixed
- Keep closed tasks completed while preserving separate automatic independent
  review tasks after both manual and agent completions.
- Return to the gateway task list after Close task or Return to queue succeeds,
  including when the action finishes after switching workspaces. Unconfirmed
  results remain visible for inspection.
- Show unadded local checkouts alongside added Local Projects, before Remote
  Projects, using greyed-out cards with an explicit Add button.

## v0.0.61 - 2026-11-02

### Added
- Submit task work through `amux submit-work` or the `submit_work` MCP tool,
  with configurable commit-and-push or pull-request submission, scoped file
  selection, attribution removal and an `AmuxID` commit trailer.
- Read-only work reports between Git refs or date windows, including task and
  review context, exact-commit CI results and explicit partial-data reporting.
- Local checkouts not yet added to amux appear greyed out at the bottom of the
  project list. Select one to add its existing folder without cloning again.

### Fixed
- Preserve separate staged and unstaged drafts during submission rebase retries
  and conflicts, with draft-safe, non-interactive recovery instructions.
- Reject pull-request submissions when selected files have upstream changes,
  preventing stale local files from reverting newer work.
- Normalize Windows submission and test-fixture paths consistently.

## v0.0.60 - 2026-11-02

### Fixed
- Reuse an existing project checkout when its repository root and origin match
  the requested project, including equivalent SSH/HTTPS origins and resolved
  local origins, instead of trying to clone it again. Conflicting directories,
  nested checkouts, files and missing origins remain errors.

## v0.0.59 - 2026-10-30

### Added
- `amux mcp`: Codex and Claude sessions started by the desktop get an MCP
  server with amux task actions plus curated agent-tools (read, symbols,
  search, Markdown, knowledge graph) and memory (search, store, get) tools.
  Turn it off with Settings > Workspaces & Terminals > Give agents amux tools.
- Release bundles include pinned, checksum-verified agent-tools v2.0.0 and
  agent-memory v1.13.9, installed and updated together with amux. The in-app
  updater in v0.0.58 and earlier installs only amux itself; the first
  MCP-enabled agent start then restores the tools from the installed release
  after verifying their checksums (`amux --update` retries if offline).
- New sessions start with relevant project memory unless the provider
  already runs the memory hook.
- Copy buttons on the instruction templates.

### Changed
- Use native internal messaging and retire legacy gateway hosting.

## v0.0.58 - 2026-10-29

### Changed
- Label the sidebar sections Local Projects and Remote Projects, show each
  remote project in its own card, and keep cards clear of the scrollbar.
- Redesign the settings window: grouped navigation on the left, a header with
  version and gateway mode, gateway modes as selectable cards, settings grouped
  in panels, and padded content that no longer clips at the bottom.

## v0.0.57 - 2026-10-29

### Changed
- Prefer registered eligible Codex agents for ordinary automatic assignments,
  waiting while they are busy. Explicit agent assignments remain supported.
- Assign completion reviews to the opposite provider whenever that provider has
  a live registration. Same-provider fallback is used only when none is registered.
- Show task details in a resizable floating drawer with wrapped text, larger
  fonts and separate comment bubbles displaying author, machine and local time.
- Cache Rust dependencies across CI runs and omit development/test debug symbols
  to reduce repeated compilation and linking overhead.

### Fixed
- Human task-board status changes remain available while review-provider checks
  enforce eligible agent claims and assignments.
- Display task and comment timestamps in the client's local timezone with the
  correct UTC offset, including daylight-saving changes.
- Include task-created and delegated-completion relay messages in the read-only
  instruction template catalog without changing delivered text.

## v0.0.56 - 2026-10-29

### Added
- The sidebar lists every gateway project not open on this machine, grayed out:
  projects checked out on other machines first, then the rest, alphabetically.
  Hovering for a quarter second shows the project, its local folder, repository,
  the machines that have it checked out and its agents. Right-click to clone it
  into the new Default Projects directory (Settings > General) or a folder chosen
  in the file browser. Desktops report their open workspaces to the server, which
  keeps each machine's last report (`PUT`/`GET /v1/checkouts`).
- A New release button in the gateway task composer fills an editable release
  task (green CI, version, changelog, publish and verify) with a line for extra
  notes. Nothing is sent until Submit Task, and existing drafts are never replaced.
- Settings has a read-only Instruction templates tab showing each instruction amux
  injects into agent sessions and the variables it fills in. Rendered text is
  unchanged.

### Changed
- The status bar counts gateway machines by hostname, so machines sharing one API
  key count separately and one machine reached two ways counts once. Desktop
  heartbeats now carry the OS and hostname, and connected machines with no agents
  count too.
- After each ordinary task assignment in a project, the next assignment goes to the
  oldest eligible queued review, so reviews no longer wait behind the whole queue.
- Agents are no longer told to pass `--agent`: notices, reminders, errors and docs
  use plain `amux tasks`, which reads the agent terminal's identity. `--agent`
  remains for headless use such as cron jobs.
- Task detail keeps Go back / Refresh and Edit / Assign / Close fixed at the top;
  only details and comments scroll, with the comment box pinned below.

### Fixed
- Agents whose project is not yet resolved count in the status bar but are never
  assigned work, protect stale claims or consume restart reminders.
- Switching or disconnecting the gateway clears the sidebar project list, and late
  replies from the previous gateway are ignored.
- On macOS, launching Codex or Claude from the desktop app no longer fails with
  `os error 2` when the provider is only on the login shell's PATH: providers now
  start through the configured login shell.
