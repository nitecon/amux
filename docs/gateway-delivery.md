# Gateway task delivery

Set the gateway URL and API key in Preferences, then enable **Send fresh gateway task notifications to agents in this app** and save. No project identifier, machine UUID or machine key is required. The app creates a durable consumer identity and resolves repositories from Git remotes and gateway metadata.

Created tasks, comments and completed tasks produce only a compact task UUID notification in a matching Claude or Codex terminal. Descriptions and comment bodies are not copied into terminal input. Events older than five minutes and events already present when a connection is established are skipped. Opening a later agent does not attach it to earlier notifications.

Each running agent has its own delivery session, pinned to its terminal, workspace and native process generation. An idle matching peer can receive a notification while another stays busy. Native prompt classification, human-input revisions, current process identity and consent are checked before input. A draft, including whitespace followed by Home, is preserved. Each attempt is saved before enqueue; interrupted or uncertain attempts are never automatically repeated. Application restart retires pending input rather than replaying it.

The task panel shows connection status, agents, pending notifications and recent receipts. `injected` means native input submission only. It does not confirm provider understanding, task claim, execution or completion. Tasks remain authoritative on the gateway.

## Built-in agent identity

amux derives a local project UUID from the normalized Git upstream and canonical
checkout directory. Subdirectories and symlink aliases resolve to the same checkout;
separate checkouts get separate UUIDs. The gateway project name is separate and
uses the lowercase repository basename, such as `amux` or `sre`. Existing verified
gateway aliases remain usable.

Each terminal receives a number when created. Its running provider uses that number
as its agent name: `codex-1`, `codex-2`, `claude-3`, and so on. The name and project
UUID determine its gateway session UUID. Execution instances and native process
generations keep old launches from claiming the identity of a replacement process.
The desktop also discovers providers started in ordinary shell tabs after `cd`.

Use the bundled `amux whoami` and `amux agents` commands to inspect identity and
local running peers. The desktop makes the bundled CLI available on terminal PATH.
Gateway task commands use the saved gateway preferences and resolve the project
automatically; environment variables and flags can override those settings. amux
owns registration and native ancestry checks. No agent-tools update, provider hook,
or actor announcement is required. An exact origin suppresses its own notification;
other matching terminals remain eligible. Named-goal events still require an
explicit matching goal and are skipped when no goal is bound.

The existing credential-bound `amux dispatch` CLI is a separate gateway protocol
and retains its documented administrator-issued machine credentials. Those
credentials are not used by desktop lifecycle notifications.

## Verification

Contract tests cover local WebSocket subscription/reconnect, offline-tail suppression, routing, durable attempts and acknowledgments, built-in identity and prompt recognition. Controlled native terminal fixtures exercise exact input, drafts, busy agents, stale callbacks and three-peer isolation. Native Windows tests run on Windows CI; installed Windows Codex receipt still needs confirmation on the user's machine. Controlled fixture input is not evidence of provider understanding or task execution.
