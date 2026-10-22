# Native reminder validation

The provisional readiness profile supports Linux with native Codex 0.163.0.
Other providers/platforms and unknown prompt versions defer. The desktop does
not yet expose consent or schedule automatic submissions.

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
