# Changelog

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