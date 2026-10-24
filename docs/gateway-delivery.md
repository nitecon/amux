# Gateway task delivery

Set the gateway URL and API key in Preferences, then enable **Send fresh gateway task notifications to agents in this app** and save. No project identifier, machine UUID or machine key is required. The app creates a durable consumer identity and resolves repositories from Git remotes and gateway metadata.

Created tasks, comments and completed tasks produce only a compact task UUID notification in a matching Claude or Codex terminal. Descriptions and comment bodies are not copied into terminal input. Events older than five minutes and events already present when a connection is established are skipped. Opening a later agent does not attach it to earlier notifications.

Each running agent has its own delivery session, pinned to its terminal, workspace and native process generation. An idle matching peer can receive a notification while another stays busy. Native prompt classification, human-input revisions, current process identity and consent are checked before input. A draft, including whitespace followed by Home, is preserved. Each attempt is saved before enqueue; interrupted or uncertain attempts are never automatically repeated. Application restart retires pending input rather than replaying it.

The task panel shows connection status, agents, pending notifications and recent receipts. `injected` means native input submission only. It does not confirm provider understanding, task claim, execution or completion. Tasks remain authoritative on the gateway.

## Native actor integration

A process delivery session and a provider-native conversation are distinct. The local actor bridge binds the invoking tool's independently registered native actor to its actual terminal process. An exact origin suppresses its own echo; other sessions using the same provider and OS remain eligible. Named-goal events require an explicitly selected matching goal. Multiple logical actors on one executor are ambiguous and do not supply a goal binding.

Desktop terminals inherit `AMUX_CLI_PATH` and `AMUX_DELIVERY_DIRECTORY`. Integrating tools call the executable directly with these commands:

- `amux actor announce`: read one versioned actor JSON object from stdin, at most 64 KiB, and return local binding context.
- `amux actor context`: return the calling native invocation's current local context.
- `amux actor peers`: return current local peer metadata.

The version 1 and 2 actor fields and UUID formulas match CMUX. Bound responses include `version`, `origin`, `provider_session_id`, `goal`, and either `base_id`/`session_slot`/`actor_id` or `executor_generation`. Local attachment fields are `workspace`, `terminal`, `provider` and `project`; `binding_state` is `bound`, `unbound` or `ambiguous`. No gateway key or terminal contents are exposed. Announcements validate the caller's native ancestry and process generation, not copied peer UUIDs. Context is private to the local user and expires after five seconds without desktop publication.

The existing credential-bound `amux dispatch` CLI is a separate gateway protocol and retains its documented administrator-issued machine credentials. Those credentials are not used by desktop lifecycle notifications.

## Verification

Contract tests cover local WebSocket subscription/reconnect, offline-tail suppression, routing, durable attempts and acknowledgments, native actor binding and prompt recognition. Controlled native terminal fixtures exercise exact input, drafts, busy agents, stale callbacks and three-peer isolation. Native Windows tests run on Windows CI; installed Windows Codex receipt still needs confirmation on the user's machine. Controlled fixture input is not evidence of provider understanding or task execution.
