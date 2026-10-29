# Native reminder validation

Current task scheduling and reminders belong to the server event router and its
Assignment, Reminder and Presence controllers. The desktop reports terminal
activity and delivers `agent.notify` through its verified native input boundary.
See [StateMachine](StateMachine.md#client-after-the-change) and
[gateway delivery](gateway-delivery.md) for the running architecture.

The previous selected-task **Remind agent** control and consented dispatch-recovery
scheduler are retired. The reusable `recipient`, `readiness`, `delivery`,
`submission` and `delegation_readiness` library modules remain, with their safety
contract tests and planned memory-context reuse. They are not wired into the
current automatic task-notification path. Earlier consent/recovery GUI artifacts
describe that historical integration, not a current application control.

Current production-path validation uses controlled native process fixtures in
`src/desktop_delegation_tests.rs`, `tests/delegation_process_contract.rs`,
`tests/terminal_contract.rs` and `tests/terminal_lifecycle_contract.rs`. These
exercise exact process generations and peer isolation, checked bracketed input,
draft preservation, buffered human input before echo, repeated notifications,
idle-gated offers, expiry, connection changes and server-reported outcomes.
They launch controlled fixtures and submit no real model turns. A queued PTY
write proves neither provider understanding nor task ownership or execution.

Restart contracts also cover current-server authority, older-router journal
fallback after capability negotiation, 404 inactivity, same-session continuity
across a fresh instance without ownership transfer, independent shared-bearer
clients, and queued receiver fences across execution/project changes.

Safety still depends on the exact current native process and terminal. Neither
screen text nor a directory match grants permission to deliver input. Human
input is tracked before its echo; an old empty-looking screen cannot make a
work offer eligible. The desktop rechecks conditions after asynchronous process
verification, preserves teed drafts, and never synthesizes Ctrl+C to interrupt a
provider. Input acceptance remains separate from controller and task outcomes.

Three optional input-only installed-provider probes exercise the retained
recipient/readiness primitives and native agent registration. They require
installed, authenticated providers. They type and clear unsubmitted input only:
no Enter, model request, gateway mutation or task claim is sent.

```sh
cargo test --locked --test installed_provider_contract -- --ignored --nocapture
cargo test --locked --test claude_provider_contract -- --ignored --nocapture
cargo test --locked --test installed_three_agents_contract -- --ignored --nocapture
```

The Codex input probe checks exact native binding, default-off consent,
buffered/partial input, placeholder styling and readiness-proof invalidation.
The Claude probe preserves the observation that typed placeholder text moved to
Home can have the same text, cursor and styles as an empty composer; its retained
readiness gate still defers that input. The three-agent probe checks distinct
registered identities, exact recipient fences, input only at the selected
terminal and retirement of one pane while peers survive.

A separate optional library-coordinator probe sends ONE real model request for
an exact response, requesting no tools or file changes:

```sh
cargo test --locked --test provider_turn_contract -- --ignored --nocapture
```

It tests the retained `submit_literal`/`ReadinessGate` path against installed
Codex 0.163.0: busy deferral, continuation readiness, checked enqueue and duplicate
suppression. It does not exercise the current desktop event-notification path.
Normal tests and CI never run these opt-in installed-provider/model-turn checks.
Neither these probes nor controlled fixtures establish deployed cross-machine
execution, generic provider readiness or automatic memory-context acceptance.

Historical evidence under `artifacts/native-turn-evidence.json`,
`artifacts/desktop-recovery-native/`, `artifacts/claude-native-input-evidence.json`
and `artifacts/three-installed-agents-evidence.json` retains its original build
and scope limitations. Retained library coordinator tests do not validate the
current desktop notification path. New acceptance evidence must identify the
exact primitive or active event delivery path and artifact it exercised; native
Windows and installed-provider end-to-end review remain separate acceptance work.
