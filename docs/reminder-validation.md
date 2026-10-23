# Native reminder validation

The provisional readiness profile supports Linux with native Codex 0.163.0.
Other providers/platforms and unknown prompt versions defer.

In a running Codex pane, enable **Allow gateway reminders + Enter**, refresh the workspace's
gateway project, then select **Remind agent #…** beside an unclaimed task.
The selection binds to that exact terminal/session/instance, even if focus later
changes. It queues literal input and Enter for the supported empty prompt. Consent
defaults off and is never saved. Changing the gateway project withdraws consent
and retires pending input; closing or exiting the recipient also retires it.

Gateway projects and query results belong to individual workspaces. A delayed
response cannot populate another workspace or a changed project binding. Queued
selections refresh task status every ten seconds; snapshots older than fifteen
seconds and failed queries pause submission. A task that leaves `todo` retires
its pending reminder. The reminder contains only canonical task/project identity
and asks the agent to fetch the current record and check eligibility before
deciding whether to claim. Task titles and notification bodies are not pasted.

With provisioned `AMUX_GATEWAY_MACHINE_ID` and `AMUX_GATEWAY_MACHINE_KEY`, a
consenting supported pane also registers its exact native executor and queries
recovery every ten seconds. This requires the complete-history contract from
agent-gateway `017ff5c` or later, including `history_complete: true`; missing or
invalid history pauses automatic reminders. Consent off performs no background
registration or recovery. Machine credentials identify the machine separately
from the executor's unique session and process generation.

A supported empty prompt permits an explicit transport receipt for eligible
pending work. Receipt alone never queues input: a subsequent fresh recovery
query must confirm it. Receipt does not claim or start work. A lost or rejected
receipt response blocks a new receipt intent; inspect `amux dispatch requests`
and the canonical gateway record before using explicit stable-ID replay.

Recovery reminders use canonical task/project identities and authenticated
prior claimant, machine and execution context when present. They ask the agent
to check existing progress, with the outcome unknown; they never authorize
restarting work. Work held by a current live peer is excluded. Delayed replies
are bound to the workspace, project revision, consent epoch, terminal and exact
native actor. Withdrawal aborts pending requests and retires pending input;
accepted gateway tasks remain on the gateway. Unsupported profiles defer.

A directory lease expiry does not prove that a local executor stopped. A CLI
executor or a consent-off pane can remain busy without heartbeats. Recovery of
another local generation requires its retained private registration record and
positive native exit or generation-replacement evidence. Missing, corrupt or
unreadable local proof defers that recovery; old installations may therefore
need explicit inspection and handoff. Of the eligible registered Codex recipients
in a fresh directory snapshot, one deterministic recipient gets that context.
This never transfers the old claim or authorizes restarting work.

The bounded delivery ledger reclaims capacity only from exact recipient
addresses whose native launcher or agent generation is provably gone. It keeps
all input fences for live or unverifiable recipients, including uncertain
attempts, and refuses new input when those fences fill its capacity.

The submission coordinator checks native identity, foreground ownership,
unread terminal input, exact styled empty prompt, settling and consent/input
checkpoints. It persists an uncertain attempt before enqueueing a bracketed
paste and Enter. A second readiness check follows persistence. Input known not
to have reached the input API can return to pending; after any input attempt,
errors remain uncertain. Duplicate attempts retain the same delivery fence.

Native input-channel acceptance is recorded as Enqueued. It proves neither
physical PTY delivery nor agent understanding, task ownership or execution start.
Further reminders wait for the unique input marker to appear before considering
another idle prompt; this is input-path observation, not semantic acknowledgment.

Two separate opt-in checks require installed, authenticated native Codex:

```sh
# Input only: never sends Enter or starts a model turn.
cargo test --locked --test installed_provider_contract -- --ignored --nocapture

# Sends ONE real model request for an exact reply, requesting no tools or files.
cargo test --locked --test provider_turn_contract -- --ignored --nocapture
```

The first checks partial input, identical placeholder text moved to Home,
buffered input while the provider is stopped, clearing and consent withdrawal.
The second uses a local transport fixture rather than a gateway task. It checks
busy deferral, continuation readiness, checked enqueueing and duplicate
suppression. Its local summary is `artifacts/native-turn-evidence.json`.
Neither check establishes cross-machine gateway handoff or platform acceptance.
Normal tests and CI do not submit provider turns.

The separate Claude input-only check requires installed, authenticated native
Claude Code 2.1.308:

```sh
cargo test --locked --test claude_provider_contract -- --ignored --nocapture
```

It binds the real native installer image (`claude/versions/2.1.308`), types and
clears input without Enter, and checks that partial text and whitespace defer.
With `NO_COLOR=1`, typed placeholder text moved to Home has the same composer
text, cursor and styles as an empty prompt; the footer changes. This evidence
does not establish a safe Claude readiness profile or busy/continuation behavior.
Claude reminder input remains disabled. The compact local result is
`artifacts/claude-native-input-evidence.json`.

A separate opt-in Linux check launches two native Codex 0.163.0 sessions and one
Claude 2.1.308 session concurrently in the same project:

```sh
cargo test --locked --test installed_three_agents_contract -- --ignored --nocapture
```

It checks distinct registered sessions, instances and agent processes, rejects
peer recipient addresses, observes unsubmitted input only in its selected pane,
then closes that pane while preserving both peers. It sends no Enter, model
request or gateway mutation. Its compact local summary is
`artifacts/three-installed-agents-evidence.json`. This is native PTY evidence;
it does not establish GUI focus behavior, gateway claim/delegation acceptance,
or Windows acceptance.

The local optimized Linux GUI check in
`artifacts/desktop-recovery-native/evidence.json` uses controlled Codex-profile
native processes and a real private gateway at `017ff5c`. Its build manifest
records the exact source base, desktop patch hash and immutable binary hashes.
Three same-project sessions have distinct IDs and generations. Consent off
registers none; human partial input prevents receipt and input. Clearing the
prompt then hiding the recipient permits one bracketed reminder and Enter only
in that recipient. Both peers remain untouched, the task stays TODO/unowned and
unstarted, and repeated queries do not duplicate input. Source tests additionally
reject late consent/project/actor callbacks and expired recovery snapshots.
These checks submit no model turns and establish neither installed-provider
understanding, native Windows readiness nor deployed cross-machine acceptance.
