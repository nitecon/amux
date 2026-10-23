# Native candidate review

This is the M1 implementation candidate, not the completed MVP or a release.
Rust/Iced with the Alacritty-backed iced_term widget is provisional. Automatic
gateway reminders are disabled; there is no claim of Ghostty or complete CMUX
parity. The review bundle's BUILD.json pins source, compiler, target and binary
SHA-256. GitHub Actions builds each platform natively, not by cross-compilation.
Passing CI is not desktop, provider or graphics acceptance.

## Windows bundle

The artifact target is x86_64-pc-windows-msvc, built on Windows Server 2025 CI.
The requested desktop review target is native Windows 11 x64. Windows 10,
ARM64, hardware graphics and provider runtime compatibility are not yet verified.
Extract the complete artifact to a writable directory. It contains two
executables: `amux.exe`, the console CLI, and `amux-desktop.exe`, the desktop
without a console window. Verify both with `Get-FileHash .\amux.exe,
.\amux-desktop.exe -Algorithm SHA256` against SHA256SUMS. BUILD.json records
them as `binary_sha256` and `gui_binary_sha256`. A missing Microsoft C/C++ runtime is a packaging finding; do not
report a successful native run if only compilation passed.

From PowerShell in the extracted directory:

```powershell
.\amux.exe --version
.\amux.exe --help
.\amux-desktop.exe --project-dir C:\path\to\project
```

`amux-desktop.exe` can also be started from Explorer; it then opens the current
directory. Keep both executables in the same directory. Agent tabs always run
`amux.exe agent ...` beside it, so agents keep a console CLI; if `amux.exe` is
missing, the tab shows a launch error instead. `.\amux.exe ... desktop` still
opens the same desktop from a console.

Shell defaults to native COMSPEC/cmd.exe, with ConPTY. WSL is not required.
Codex and Claude buttons require their official native commands on PATH;
amux neither installs providers nor changes their permission settings. CLI
agent launch is `.\amux.exe --project-dir C:\path\to\project agent codex`
or `agent claude`. Flags after `--` are forwarded as literal arguments.

If the graphics backend fails, record that failure, then separately try the
software path with `$env:ICED_BACKEND = 'tiny-skia'`. A software-path pass does
not establish hardware graphics acceptance. `amux-desktop.exe` has no console,
so its startup errors are not visible; if it fails to open, run
`.\amux.exe --project-dir C:\path\to\project desktop` from PowerShell to see
the error. There is no installer, Start menu entry or icon yet.

## Desktop gateway configuration and tabs

Open **Preferences** from the left sidebar, or **Gateway preferences** in the task
panel. A separate native window has **Gateway** and **Updates** tabs. Enter the
HTTP(S) gateway origin (without a path) and API key, then **Save preferences**.
Each workspace resolves its project automatically by matching its Git origin
against gateway canonical repository metadata. Missing mappings are registered
using the same repository-registration API as CMUX and verified by rereading the
project list. Ambiguous mappings are refused; folder names are never guessed.
There is no editable project identifier. These desktop settings persist in
`gateway-preferences.json` in the application data directory (or `AMUX_STATE_DIR`).
Keys are masked in the form and the file is private to the current user on Unix;
on Windows it inherits the application's data-directory access permissions.
Saved settings take precedence over environment values for the desktop.
The CLI continues to use its documented flags and environment variables.

Machine UUID and machine key are optional for listing/creating tasks. Obtain
provisioned values from the gateway administrator for supported recovery; saving
these fields does not enable Windows or Claude automatic reminder input.
Changing the connection clears old task lists and withdraws reminder consent.
Pending or unconfirmed task submissions must be resolved before changing it.

Check saving and reopening preferences, authentication failures and task refresh.
Closing preferences must leave the main window and terminals usable; closing
the main window must also close preferences. In **Updates**, direct-download
installs offer **Update now** and then an explicit restart, without a command-copy
step. Package-manager installs retain their manager-specific instructions.
Tabs use simple Terminal/Codex/Claude names, a directory subtitle, and an **×**
within each tab header. Hover the subtitle for full path and terminal title.
Verify that closing a hidden tab or a tab in another split leaves peers intact.

## Linux bundle

The x86_64-unknown-linux-gnu artifact is built on Ubuntu 24.04. It is an initial
review artifact, not a Debian 12 portability promise. Run
`chmod +x ./amux ./amux-desktop`, then `./amux-desktop --project-dir
/path/to/project` (or `./amux --project-dir /path/to/project desktop`). Keep both
binaries in one directory; agent tabs run the sibling `amux`. A native X11/Wayland display
and system graphics/font libraries are required. The Linux owning session also
ran an isolated Xvfb/software-renderer smoke; that is not equivalent to IME,
accessibility or hardware acceptance on a user's desktop.

## Session identity and recovery

Each explicit agent launch creates a unique amux session UUID and execution UUID.
These are launch identities, not provider-native conversation IDs. `amux whoami`
prints inherited identity; `amux agents` lists registered live launches. Task
mutations additionally verify registered process generation and caller ancestry,
rejecting unrelated callers and unwrapped nested agents that inherited another
agent's environment. Start nested agents with `amux agent ...` for their own
identity. Native provider-wrapper variants still require platform verification.

Linux generation uses boot identity and raw process start ticks; Windows uses
native creation FILETIME. Layout snapshots contain workspace/split/tab metadata,
not task state or credentials. Restored terminals and agents stay stopped until
explicit launch; prior tasks are never automatically claimed or rerun. Scrollback,
full window fidelity and provider-native continuation binding are not implemented.
Use a fresh AMUX_STATE_DIR to isolate a review from other candidate sessions.

Gateway reads use AMUX_GATEWAY_URL, AMUX_GATEWAY_KEY and AMUX_GATEWAY_PROJECT.
For example, `amux tasks --project amux --gateway-url https://gateway.example
list`. Run task mutation commands from the launched agent's project directory.
Claims require the gateway to return the exact session and instance as owner;
there is no fallback to project/provider/machine names. `started` checks current
ownership and appends an explicitly non-atomic agent report comment, not a new
authoritative gateway lifecycle state. No submission retries are automatic while
server request-id deduplication remains unverified. Never include keys in reports.

## Feedback required before accepting milestones

- Record source revision, artifact/hash, Windows/Linux version, graphics backend,
  provider versions and exact commands. Distinguish source review, CI build and
  native runtime observations.
- Exercise shell input/output, Unicode/IME, clipboard, resize, tabs, both split
  axes and focus. Use different markers in different PTYs; verify an exited or
  hidden pane never redirects events or closes another focused pane.
- Launch three agents in one project, including two of the same provider. Verify
  distinct identities, explicit task ownership, competing claim denial, wrong
  session completion/release denial and restart fencing against an agreed gateway
  fixture. No real work should be rerun just to test recovery.
- Test close and quit with child PID/generation evidence, including a direct
  agent that ignores HUP and a busy/hidden tab. Linux now fences the direct agent
  with parent-death SIGKILL and preserves ordinary foreground Ctrl-C. Bounded
  descendant retirement and native Windows teardown are still open findings;
  the direct-child test does not prove arbitrary descendant cleanup.
- Check layout recovery without launching old processes and terminals remaining
  usable without a gateway. Report missing capabilities and regressions with
  file/line or runtime evidence. Keep M2 unapproved until safe readiness, exact
  recipient, consent, deduplication and actual gateway handoff are proven.
- With a controlled gateway fixture, check the desktop task form: multiline
  context, one POST after a double click, an original-project receipt after
  switching projects, and an unknown result without automatic retry. Desktop
  user submissions must not be attributed to a running agent.
- Enable **Notify on process exit** and test a local terminal with no gateway.
  The setting starts off on every launch and applies to future process exits.
  A missing or unresponsive notification service must leave other terminals and
  accepted task receipts usable. Windows uses a notification-area balloon and
  an amux tray icon while the notifier lives; OS settings may suppress display.
  Service acceptance does not prove the notification was visible. See
  [notification validation](notification-validation.md).

Linux review: CMUX task `01a164c9-f932-7b51-98ef-f4757d4202c9`.
Windows review: CMUX task `01a164c9-fac2-7ec1-8c8c-b62a62b0d016`.
MVP parent: amux task `01a164bf-0ee3-7222-9fad-5bad89aae754`.
