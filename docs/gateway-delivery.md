# Gateway task delivery

Set the gateway URL and API key in Preferences, then enable **Send fresh gateway task notifications to agents in this app** and save. No project identifier, machine UUID or machine key is required. The app creates a durable consumer identity and resolves repositories from Git remotes and gateway metadata.

Created tasks, comments and completed tasks produce only a compact task UUID notification in a matching Claude or Codex terminal. Descriptions and comment bodies are not copied into terminal input. Events older than five minutes and events already present when a connection is established are skipped. Freshness uses the gateway upgrade response’s HTTP Date plus monotonic elapsed time when available, so a local PC clock offset does not incorrectly expire a fresh event. Opening a later agent does not attach it to earlier notifications.

Each running agent has its own delivery session, pinned to its terminal, workspace and native process generation. Automatic delivery uses the owned terminal and process identity, with no screen parsing or prompt/footer matching. amux tees typed text and subsequent editing keystrokes into an in-memory buffer. Enter resets the buffer; modifier/navigation keys alone do not count as text.

A notification does not wait for an empty prompt or for a draft to be submitted. If user text is buffered, amux clears the provider draft, sends and submits the notification, then restores the buffered input in the same ordered terminal write. A native End key separates the message from Enter so a character-paste burst is flushed without a timer. Terminal protocol flags determine bracketed-paste encoding without reading rendered content. The current process, consent and event freshness are checked before the transaction. Each attempt is saved before enqueue; interrupted or uncertain attempts are never automatically repeated. Application restart retires pending notifications rather than replaying them.

The task panel shows connection status, agents, pending notifications and recent receipts with the reason for each delivery decision. Gateway acknowledgment
confirms that the decision was saved, including a skip; it does not imply delivery.
`injected` means the notification input transaction was enqueued. It does not confirm provider understanding, task claim, execution or completion. Tasks remain authoritative on the gateway.

## Built-in agent identity

amux derives a local project UUID from the normalized Git upstream and canonical
checkout directory. Subdirectories and symlink aliases resolve to the same checkout;
separate checkouts get separate UUIDs. The gateway project name is separate and
uses the lowercase repository basename, such as `amux` or `sre`. Existing verified
gateway aliases remain usable.

Amux assigns a name to each terminal from a built-in pool of 500 technology
contributors across the eras, with Will Hattingh at entry 251. The name is saved
with the assignment and displayed with the provider, such as `Alan Turing - Codex`.
Terminal numbers and project UUIDs continue to determine distinct internal session
IDs. After the pool wraps, a numeric suffix keeps names distinct. The desktop also
discovers providers started in ordinary shell tabs after `cd`.

Each gateway notification includes its recipient's assigned ID and a command such
as `amux --agent <id> tasks get <task-id>`. Use the same `--agent` ID for `whoami`,
claim, comment and completion. Task commands resolve that stored assignment
directly, without OS ancestry lookup. This also works when a provider filters the
terminal's environment variables. Assigned launch environment values remain
supported. An unknown or ambiguous assignment is rejected.

Use the bundled `amux agents` command to inspect local running peers. The desktop
makes the bundled CLI available on terminal PATH.
Gateway task commands use the saved gateway preferences and resolve the project
automatically; environment variables and flags can override those settings. amux
owns assignment and registration. No agent-tools update, provider hook,
or actor announcement is required. An exact origin suppresses its own notification;
other matching terminals remain eligible. Named-goal events still require an
explicit matching goal and are skipped when no goal is bound.

The existing credential-bound `amux dispatch` CLI is a separate gateway protocol
and retains its documented administrator-issued machine credentials. Those
credentials are not used by desktop lifecycle notifications.

## Verification

Contract tests cover local WebSocket subscription/reconnect, offline-tail suppression, routing, durable attempts and acknowledgments, and built-in identity. Controlled native terminal fixtures exercise exact input, immediate delivery with restored user drafts, text-versus-navigation tracking, hidden footers, unrecognized layouts, stale callbacks and three-peer isolation. Native Windows tests run on Windows CI; installed Windows Codex submission still needs confirmation on the user's machine. Controlled fixture input is not evidence of provider understanding or task execution.
