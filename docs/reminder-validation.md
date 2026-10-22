# Native reminder validation

The provisional readiness profile supports Linux with native Codex 0.163.0.
Other providers/platforms and unknown prompt versions defer.

In a running Codex pane, enable **Allow reminder input + Enter**, refresh the workspace's
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

Automatic assignment discovery, session-start recovery and previously claimed
task reminders remain incomplete. Claimed recovery needs authoritative prior
claimant/machine provenance from the gateway; the UI does not infer that from a
task's originating hostname. Explicit task selection is the current routing
boundary. Unsupported profiles keep the selection deferred.

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
