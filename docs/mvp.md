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

**Status:** Linux/Codex 0.163 supports explicit task reminders and a consented
recovery bridge. Consent defaults off and is not saved. Provisioned machine
credentials enable exact native executor registration, fresh bounded recovery,
readiness-gated transport receipt and deduplicated reminders. Receipt, input,
agent understanding, claim and execution remain separate. Claude and Windows
readiness, deployed cross-machine handoff and independent acceptance remain
open. See [reminder-validation.md](reminder-validation.md).

The native CLI consumes credential-bound exact-session and offline machine
delegation, receipt, claim, start, outcome and recovery operations. Private real
gateway checks at `f060414` and `1b3f4b4` passed independent native executor
fixtures, wrong-peer rejection, stable-ID recovery after a lost reply, concurrent
receipt, explicit handoff and prior-generation ownership fences. The latter
exposed missing history for a new destination machine; gateway `017ff5c` adds
complete authenticated history and its explicit completeness marker. That
source is in draft agent-gateway PR #4 with green source CI; it is not deployed.

The optimized local desktop bridge check against private `017ff5c` passed three
same-project native fixtures, consent off, partial-input deferral, hidden exact
recipient delivery, peer isolation, receipt without claim/start and repeated
query deduplication. Its source/patch and binary hashes are recorded with the
local evidence. Source tests cover late callbacks, withdrawal, project changes
and expiry. These fixtures are separate from installed-provider, Windows and
deployed cross-machine acceptance.

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

Both owners have source fixes. Gateway PR #4 remains draft and undeployed;
the agent-tools resolver fix has been verified through its repository-local
binary and has not been installed globally. amux requires the documented server
guarantees and defers when they are absent.
