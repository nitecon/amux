# amux MVP plan

This document defines the MVP milestones, what evidence closes each one, and the
gates that remain. Requirements come from [features.md](features.md), boundaries
and owner decisions D1–D6 from [architecture.md](architecture.md), and stack
evaluation from [tooling.md](tooling.md). The gateway task board is the source of
truth for live status; the status lines here were written on October 22, 2026.

## Ground rules

- **Provisional stack.** The candidate is Rust with Iced 0.14 and a repository-local
  copy of iced_term 0.8 (Alacritty terminal engine). It exists to prove the
  architecture with real PTYs. It is not the selected stack, and there is no
  Ghostty integration or CMUX parity claim. The tooling evaluation gates still apply.
- **One identity per running session.** Every running Claude or Codex session gets
  its own session and instance identity, including several sessions of the same
  provider in the same project on the same machine. Tasks are claimed and delegated
  by that exact identity, never by project, provider or hostname. These are amux
  launch identities, not provider-native conversation IDs.
- **Never replay work on restart.** Restored layouts bring back panes and tabs with
  every process stopped. Nothing is relaunched, rerun or claimed automatically
  (D4).
- **The gateway owns tasks (D5).** amux reports only what the gateway acknowledged.
  A claim, a reminder, or a terminal write is never treated as execution start or
  completion.
- **Builds are not acceptance.** Passing CI or a screenshot proves a build, not
  runtime behavior on a user's desktop.

## Command line

The shipped `amux` binary works without agent-tools installed:

| Command | Purpose |
| --- | --- |
| `amux --project-dir <dir> desktop` | Native desktop (also the default) |
| `amux agent codex\|claude [-- args]` | Launch an agent with its own session and instance identity |
| `amux whoami` | Print the calling agent's identity |
| `amux agents` | List live amux-launched agents |
| `amux tasks list\|get\|create\|claim\|comment\|started\|done\|release` | Gateway tasks |
| `amux messages list\|get\|send` | Inline scratch text through gateway messages |

Gateway access uses `AMUX_GATEWAY_URL`, `AMUX_GATEWAY_PROJECT` and
`AMUX_GATEWAY_KEY`. Mutations must come from an amux-launched agent in its
original project directory. Details are in [gateway-cli.md](gateway-cli.md).
Task cancellation is not available until the gateway supports it.

## Milestones

### M1: native terminal and three-session identity

**Goal:** a runnable desktop with real PTYs, workspaces, tabs and splits, and
distinct identities for agents it launches.

**Closes when:**

- Shell input and output, resize, tabs, both split axes and focus work natively
  on Linux and Windows. Events for one pane never reach another, including
  hidden and exited panes.
- Three agents in one project, two of them the same provider, get distinct
  identities, and claims cannot overwrite another owner.
- Closing a tab or quitting retires the processes amux owns, including a direct
  agent that ignores HUP, without touching other terminals.
- A restored layout launches nothing.

**Status:** candidate builds published. The Linux review requested changes. L1
and L2 are fixed. Native CI covers bounded Linux terminal-session cleanup and
Windows Job Object cleanup after enrollment. Late forks, abrupt desktop death,
Windows children created before enrollment and complete native teardown
acceptance remain open. An optimized native Linux GUI probe launched two
installed Codex agents and one Claude agent with three distinct identities.
Unsubmitted input appeared only in the selected Codex tab; closing it preserved
both peer process generations, and normal window close retired the remaining
agents. This is local evidence, not gateway ownership or independent platform
acceptance. The Windows review covered the CLI, provider
launches and a responding window. Its interactive checks did not run because
screen-control approval timed out.

The independent Linux review of `014ff96` verified packaging, shell routing,
layout restore and task receipts, and requested exit diagnostics and contained
tab headers. These fixes are in `39de3d6`; local optimized GUI checks confirm
both, with bounded final visible text retained after the PTY closes. `b4454c5`
also ends closed terminal event subscriptions. Independent review of the new
bundle and native Windows interactive evidence remain required.

### M2: gateway handoff and exact-agent ownership

**Goal:** shared task operations and reminders delivered through the terminal,
with no dependency on provider or agent-tools hooks (D6).

**Closes when:**

- Readiness, consent and partial-input protection are proven before any
  automatic input. Unknown providers, versions and modes defer.
- Pending tasks are recovered by fresh gateway queries. Transient events older
  than five minutes are never replayed.
- Process-generation fences survive restart and reconnect, and duplicate
  reminders are suppressed.
- Cross-machine handoff works against the gateway: a Linux agent delegates to
  Windows while Windows is off, then Windows reconnects and gets a reminder.
- Gateway gaps that block these are fixed by agent-gateway, not worked around
  in amux.

**Status:** reminders to a task the user explicitly selects work on Linux with
Codex 0.163. That covers per-pane consent (off by default), exact recipient
binding, readiness checks and a persistent reminder record. Automatic
assignment, recovery of previously claimed work, Claude and Windows readiness,
and cross-machine handoff are not done. See
[reminder-validation.md](reminder-validation.md).

The native CLI now consumes credential-bound exact-session registration,
delegation, receipt, claim, start, outcome and recovery operations. A private
real gateway at `f060414` and amux at `b4454c5` passed three native fixture
executors, wrong-peer rejection, stable-ID recovery after a lost reply, explicit
handoff and prior-generation claim inspection without adoption. These fixtures
are separate from installed-provider and deployed cross-machine acceptance.
Gateway offline machine delivery is implemented separately at `1b3f4b4`;
amux client integration, desktop reminders and platform acceptance remain open.

### M3: cross-platform MVP acceptance

**Goal:** package and validate the complete MVP on Linux and native Windows.

**Closes when:**

- M1 and M2 are accepted, and all CMUX review findings are resolved or
  explicitly deferred by the owner.
- Packaging works on both platforms, including a GUI entry point.
- Unimplemented parity items from features.md are listed explicitly. The current
  map is [parity-status.md](parity-status.md).
- macOS stack feasibility has a recorded result, even if macOS ships later.

**Status:** packaging and review are in progress. Separate CLI and GUI entry
points are built on Linux and Windows; native CI and bundle hashes are verified.
Interactive platform acceptance and M1/M2 approval remain open. The recorded
parity inventory has 29 rows: 1 implemented, 9 partial and 19 missing. macOS
feasibility is recorded in [parity-status.md](parity-status.md).

## Evidence required from CMUX reviewers

Linux and Windows CMUX agents review concrete artifacts, not source alone. Each
report records:

- The source revision and artifact SHA-256, checked against the bundle's
  `SHA256SUMS` and `BUILD.json`.
- OS version, graphics backend, provider versions and the exact commands run.
- Which observations come from source review, which from CI, and which from a
  native run.
- For each milestone criterion above: pass, fail or not tested, with the reason.
- Findings with file and line or runtime evidence, numbered so fixes can refer
  to them.

Step-by-step bundle checks are in [windows-review.md](windows-review.md). Local
evidence under `artifacts/` is ignored by git. Summarize it in the review task
rather than linking local paths. Reports never include gateway keys.

## Dependencies on other projects

amux does not edit other repositories. These gaps are delegated to their owners:

- **agent-gateway:** exact-session claims, session-targeted delegation, request
  deduplication, recovery of accepted tasks with prior-claim history, and an
  explicit cancellation that preserves the task.
- **agent-tools:** resolving the identity of a Claude session started by another
  agent, so nested sessions can claim work.

Until these land, amux fails clearly instead of assuming server guarantees.
