# Operations

See the [getting started guide](../README.md) for downloads and first launch.

## Gateway and Discord

For a local gateway with bearer authentication, build the CLI and set `GATEWAY_API_KEY`.

```sh
./bin/amux server run --database /path/to/amux.db --listen 127.0.0.1:7913
```

Discord messaging is opt-in. Add `--discord-token-env NAME` and
`--discord-guild-id ID` to `server run`; optionally add
`--discord-category-id ID` for newly created rooms. The token is read from the
named environment variable. Enabling Discord does not change the server's
`internal` default channel. The CLI configures Discord outbound messaging and
inbound room ingestion; other channel adapters remain pending. The inbound
source restores registered room cursors, journals messages before advancing
their project cursor, and bounds its producer queue. Source-contract coverage
and remaining limitations are tracked in [go-parity.json](go-parity.json).
Live Discord provider and platform runtime acceptance are not established.

Container and systemd setup is documented in [deploy/README.md](../deploy/README.md).

### Go gateway enrollment and Rust cutover

For a networked Go gateway, use a server profile and mTLS. The hostname or IP
advertised to clients must be in the server certificate, and clients must
reach both the secure and discovery ports.

```sh
./bin/amux server run --profile "$server_profile" --secure-instance-id "$server_instance_id" \
  --listen 0.0.0.0:24182 --discovery-bind 0.0.0.0:24181 --profile-tls-name gateway.example
```

On the server, `./bin/amux server peering-key --profile "$server_profile"`
generates an invitation that expires in ten minutes. Use a fresh invitation per
client and provide it on stdin to `./bin/amux client enroll --state-dir "$client_state"
--server https://gateway.example:24182`. Pairing pins the cluster root, issues
a client certificate, and returns the path to the enrolled manifest. Start the
desktop with `./bin/amux-desktop --enrolled-profile "$client_manifest"`. The manifest
contains the gateway address, and the client renews its certificate over mTLS.

For a same-host Rust cutover, stop the Rust gateway before starting Go against
its server profile. Back up the complete profile and retain the original server
installation ID, endpoint, and ports. The application database contains tasks
and conversations. The security database and root certificates and keys carry
the installation trust and enrollment state. Existing clients can reuse their
enrolled manifests. Keep the server profile and client credential files at
their existing locations and protect them as private keys.

## Task and project commands

Console task commands use the same guarded gateway API and emit JSON on stdout:

```sh
./bin/amux tasks --server https://gateway.example --project demo list
./bin/amux tasks --server https://gateway.example --project demo --human \
  create "Prepare release" --root-reason user-directed --specification "User requested a release review."
./bin/amux tasks --server https://gateway.example --project demo \
  --session SESSION_UUID --instance INSTANCE_UUID --provider codex --os linux \
  --goal release claim TASK_UUID
./bin/amux tasks create --help
```

Writes require a complete native execution identity or explicit `--human`;
`claim`, `started`, and `cancel` require native identity. Supported operations include
`list`, `get`, `create`, `claim`, `started`, `rank`, `set-os`, `comment`,
`user-review`, `done`, `release`, `confirm-mutation`, `set-changeset`, `cancel`,
`cancellation`, `delegate`, `update-delegation`, `watch`, and `submit-work`.
Keep server/project/identity options before the operation; operation flags can
follow its positional arguments. Native goal-scoped lists and claims require
the selected goal. Mutations are never automatically retried. Workspace/profile
discovery, legacy agent aliases, and report remain incomplete.

`tasks delegate TITLE --parent UUID --target-project IDENT --specification TEXT`
creates a source tracking task and an unassigned target task atomically. It
requires native identity and a source-project parent. Target completion makes
the tracker available for inspection; only closing that tracker after the target
is done accepts the handoff. Native MCP exposes the same operation as
`task_delegate`, using the conversation task as its default parent. Inspect both
projects before retrying an unconfirmed handoff. `tasks update-delegation TASK_ID`
edits the source contract and mirrors its title, description,
specification, and labels to the linked target. The update, paired system notes,
and target-project nudge record are atomic. A shared-tick inbox plugin delivers
project nudges into an eligible native conversation. Source comments are mirrored to the target;
target comments are not echoed back. The SDK pins both linked task IDs before
the update, validates the receipt, and does not retry uncertain writes. Native
MCP exposes this as `task_update_delegation`. The Fyne editor continues to use
the source-compatible generic selected-task edit path; the dedicated paired
contract update remains a separate CLI/MCP operation. Credential-bound
dispatch/session handoffs remain pending.

Use `--notify-results` on `tasks create` or `tasks delegate` to subscribe the
calling execution to results. `tasks watch TASK_ID` sets that subscription;
`tasks watch TASK_ID --clear` removes it. The caller must have a current registered
target, which native workers advertise automatically. A requester is separate
from the task's executor. Queued results are checked again before delivery, so
removed or changed subscriptions do not receive stale prompts. Native MCP exposes
`notify_results` on creation and the `task_set_requester` tool. These writes check
server capability and exact acknowledgements and are never automatically retried.

Native project inbox access uses `GET /v1/projects/{ident}/messages/unread`
and `POST /v1/projects/{ident}/messages/{id}/confirm` with complete native
execution headers. Acknowledgements belong to the session, so one agent reading
a message does not hide it from another. The SDK checks project IDs, ordered
message IDs and explicit confirmation responses. If a confirmation response is
lost, inspect unread messages before retrying. The shared-tick inbox plugin
requires one live, trusted native session and an unambiguous conversation. It
defers unsent drafts and full queues, and queues messages behind a running turn.
The prompt and per-session acknowledgement commit together; acknowledgement
means durable queue acceptance, not completed model execution. Source-contract
coverage and validation evidence are tracked in [go-parity.json](go-parity.json).

Human project messages use `POST /v1/projects/{ident}/messages/human`; replies
resolve the thread first and validate the returned parent, including the
source-compatible one-hop behavior for nested and orphaned replies. Messages
are stored before channel delivery is reported. The internal channel returns
`delivered: true`; an unavailable external channel returns a delivery error
while leaving the message available in unread inboxes. The `author` value is a
display label, never an agent identity. This Go slice uses API-key caller
identity for human posts: browser user-session attribution and message
thread-list/inbox UI remain pending.

`GET /v1/projects/{ident}/messages` returns root-thread summaries and
project-wide counts independently of the selected page and filters. State,
comma-separated author kinds, literal `agent_id` (including an explicit empty
value), limit and offset follow the source query behavior: an omitted limit is
50, values cap at 500, and explicit zero uses the storage helper's default of
100. Unknown state uses the unresolved default. Empty author-kind strings are
preserved; unsigned numeric query values accept an encoded leading `+`, and
duplicate scalar query parameters are rejected. The source bulk
alert-resolution helper is internal storage behavior;
there is no bulk-resolution HTTP route. Native agent messages are available
for internal projects through `POST /v1/projects/{ident}/messages`,
`POST /v1/projects/{ident}/messages/{id}/reply`, and
`POST /v1/projects/{ident}/messages/{id}/action`. `POST /v1/projects/{ident}/room`
returns the internal project room. These writes require a complete native
origin; sender identity comes from its session, and storage validates project,
parent, nonblank body, then channel before persisting the message and sender
confirmation. The body remains literal. Omitted hostnames default to the native
session ID, and only the route's selected `content` or `message` alias is
type-checked. The SDK validates message, parent, and internal receipt IDs and
does not retry uncertain writes. For a configured external channel, agent
messages use structured provider sends before storage; a failed send creates no
agent message, and an uncertain send is not retried. Human posts are stored
first, then sent as raw text; a failed delivery keeps the inbox row and reports
`delivered: false`, while success stores the provider ID on that row. Neither
path automatically retries an uncertain provider result. Repository projects
using an external channel require a configured provider and create their room
at registration. External adhoc projects may register without a provider and
keep an empty room until `/room` or a send lazily provisions it. Without a
provider, room creation and agent sends return 503; registering an external
repository with no configured provider returns 400 without creating it.

Inbound and outbound source-contract coverage, validation evidence, and limits
are tracked in [go-parity.json](go-parity.json). Live Discord provider and
platform runtime acceptance are not established.
Thread-list/inbox UI remains pending.

The CLI can list message threads, read a full thread by its root message ID, and
send an inline message:

```sh
./bin/amux messages --gateway-url https://gateway.example --project demo list --limit 50
./bin/amux messages --gateway-url https://gateway.example --project demo get 81
./bin/amux messages --gateway-url https://gateway.example --project demo send --subject 'Review handoff' -- 'Review context and findings, inline.'
```

Reads require gateway credentials but no worker identity. Sending requires the
complete native worker identity and the same canonical checkout from which that
worker was launched; workers provide this context automatically. Publication
does not claim or start a task, and uncertain sends are not automatically
retried. The source display-alias header and legacy manual-agent selection are
not yet implemented. Source-contract coverage and validation evidence are
tracked in [go-parity.json](go-parity.json).

Work submission runs on the agent's explicit checkout, after checking the active
task's exact owner, project binding and selected goal. It commits only the listed
changed files, preserves unrelated drafts, publishes the result, and posts an
agent-authored task comment. It does not complete the task. The same engine is
available through the native MCP `submit_work` tool and the top-level
`amux-go submit-work` alias.

```sh
./bin/amux tasks --server https://gateway.example --project demo \
  --session SESSION_UUID --instance INSTANCE_UUID --provider codex --os linux \
  submit-work --directory /path/to/checkout TASK_UUID "Fix the parser" src/parser.go
# For large selections, supply a JSON array instead of expanding command arguments:
# submit-work --directory /path/to/checkout --paths-file /path/to/paths.json TASK_UUID "Move assets"
./bin/amux work-submission show
./bin/amux work-submission set --mode pull_request --branch-prefix review/
```

The default mode is `commit_and_push`; PR mode uses a branch with the full task
UUID and a temporary index so the caller's checkout stays unchanged. It retains
the Rust `commit-tree` path, which does not run commit hooks. Settings live in
`amux/work-preferences.json` under the OS configuration
directory. Override it with `--work-preferences PATH` or
`AMUX_WORK_SUBMISSION_PREFERENCES`. Updates preserve unrelated fields in a
Rust-compatible preferences file. Native workers pass the pinned checkout and
configured preference path to their MCP child. The managed settings contain no
credentials; Git and `gh` use the host's configured noninteractive authentication.
The Fyne browser's **Work submission settings** section edits this same file
with explicit Save and Cancel. Desktop `--work-preferences PATH` selects the
file for both the panel and hosted workers; saving does not publish work.

If publication fails after committing, the error retains the local task commit
and recovery instructions. After resolving the checkout, use
`--resume-commit SHA` with the same task and exact paths to publish without
committing again. An unconfirmed result comment retains the published reference
and is not automatically retried. Large result comments summarize omitted paths;
the JSON receipt keeps the full file list. Local Git publication and the gateway
comment are separate effects, guarded by an ownership preflight rather than a
transaction spanning both systems.

`cancel TASK_ID --reason TEXT [--changeset JSON]` records cancellation and
preserves the prior claimant, start time and history. New cancellation requires
explicit changeset confirmation; an identical reporter/reason replay returns
the original report. Read it with `cancellation TASK_ID` after an uncertain
reply. The report always carries `executor_stop_confirmed:false`. Startup widens recognized legacy task status constraints after saving a
protected SQLite snapshot beside the database. The rebuild preserves rows,
comments and schema objects; migration 14 adds the cancellation report table.

Native project registration, repository mapping, archive/restore, ordinary task
creation/listing/details/comments, claims/releases, task-event replay, and the
raw task WebSocket stream are ported in part. Guided file-change confirmation,
completion, parking for user decisions, and code/documentation review generation
are also available. Task snapshots publish transactionally, and durable review
deliveries preserve obligations without blocking task completion. Streaming
uses `github.com/coder/websocket` with durable cursors and connection fences.
Plugins now have a shared channel bus with independent bounded subscriptions.
The server publishes 30-second ticks for reconciliation; plugins read
committed state through owned subscriptions. Slow subscribers must resubscribe and
reconcile. The immutable event registry and durable event/pending-delivery journal
now validate subscriptions, persist atomically and reject conflicting UID retries.
The callable runtime now handles execution phases, dependencies, retry/FIFO
recovery and bounded owned work. Each registered plugin has its own bounded
work inbox; the global active-work cap and FIFO/dependency decisions remain
runtime/journal controlled. Transactional task-outbox draining and shared
clock and native presence source libraries now exist. Presence transitions
retain native identity/capabilities, causal clock events and offline task ownership.
The native assignment plugin waits for Presence and preserves the existing FIFO,
platform and reviewer gates without reconciling Presence itself.
Its claim, explanation comment, notification and captured offer commit together.
Delivery results must match the captured source, target and notification ID.
Failed delivery can release an untouched, unadopted claim; native adoption or
conversation evidence prevents release. Refusals suppress reassignment until
output changes or the session leaves. A callable native-delivery plugin now
reuses the adoption prompt for real offers and queues reminders and recipient
notices in the exact native conversation. Receipts freeze receiver identity and
authenticated source; replay cannot create a second prompt or result. Busy
provider turns and reported drafts defer idle-only notices, while task ownership
alone does not block them. Delivery means durable intent, not task completion.
`amux server run` now acquires the complete builtin registry before announcing
readiness and owns its router, task/presence/heartbeat sources and shared clock.
The authenticated `/v1/events/stream` forwards task and presence events,
notifications and retry notices. Consumers retain an acknowledged replay cursor
for 24 hours, receive one event at a time and resume unacknowledged events after
reconnect. A new connection fences the previous connection's acknowledgements
and cleanup. Remote emits use the server-derived `client:api-key` source and
cannot grant native execution authority or emit server-only events.
The durable review plugin consumes task completion events and shares the existing
code/documentation review transaction, with stale completion guards and retries.
The shared agent notification contract now validates notification/result events
and snapshots registered sender and owner hosts when emitting notifications.
Worker presence advertises optional host metadata with the same sequence fence.
Explicit task-target registration now validates repository binding and keeps a
separate 120-second manual routing lease. Native activity reports cannot renew
that lease; registration cannot extend activity liveness or replace its source.
Native pools register their current execution against the checkout's actual Git
remote before reporting availability. Invalid bindings stop provider admission
while preserving active task ownership and the saved run journal.
Native and retained event observations now use one live authority, with separate
native and global event sequence fences. Unknown legacy observations remain
visible without gaining native execution permissions. Protected migration keeps
the Rust archives and unrelated history; event activity cannot renew routing leases.
The source-case map now covers all 14 Rust presence contracts with adapted tests.
The inbound plugin commits channel messages and checkpoints together, deduplicates
provider redelivery and preserves FIFO through failures and restart. The reclaim
plugin reads committed Presence on shared ticks and releases stale legacy claims;
native ownership and offline execution journals remain protected after expiry.
The presence summary counts current agents, busy agents, machines and
platforms, including machines with empty inventories. Accepted source metadata
survives heartbeat draining and restart; its lease uses the shared monotonic clock.
Accepted native reports also capture durable lifetime heartbeats. The reminder
plugin waits for Presence, nudges quiet work, and sends startup continuity once
per execution and client lifetime without transferring task ownership.
The notifier library routes task activity to current owners and requesters,
including OS, goal and generation checks, with transactional replay receipts.
Retention cleanup preserves unanswered human messages, recent offline replay,
unfinished deliveries and native outbox causes. Active completion acknowledgments
remain fenced until their matching finish succeeds.
The production router replaces the legacy assignment and review loops. Existing
review retry obligations move transactionally into durable completion events;
review uniqueness prevents duplicate children. Shutdown cancels and joins the
router, its sources and active streams before closing storage. Older delivery
constraints upgrade with a backup while preserving retry progress.
`GET /v1/plugins` and `PUT /v1/plugins/{id}` expose the five existing switches:
assignment, notifier, reminder, review and reclaim. Message retention defaults to
30 days (bot messages: 7); use `--message-retention-days` and
`--bot-message-retention-days` to change these values.

The native browser's **Server plugin settings** panel reads and changes these
server-wide switches. Changes save immediately after a confirmed response;
pending requests disable the controls. An unconfirmed write is never replayed:
use **Reload settings** to read the server before trying another change. Review
changes apply to future completions; already queued reviews remain available.
Review claims now prefer eligible independent reviewers and the opposite provider.
Native task PATCH supports title, goal, description, specification, hostname,
labels and rank edits, including edits combined with a workflow transition.
Goal, OS and review metadata guards apply inside the same transaction; guided
confirmation preserves the requested edits without committing a preview.
Dispatch/delegation, legacy owner assignment and full desktop behavior remain unfinished. Startup locks the database profile before retiring old
connection leases, and shutdown closes streams before storage.
The native ACP adapter supports text/tool events, explicit permission replies,
exact session resume, and cooperative cancellation through stdio. Subprocess
fixtures verify those exchanges without login or model calls. Durable task
conversations now have an HTTP API and a read-only WebSocket stream with sequence
replay, duplicate suppression, native session binding, and worker instance fences.
Integration tests persist ACP events before delivery. Durable prompt, permission,
and cancellation commands now have HTTP routes and live revision replay. Claims
are fenced to the current worker instance, and prompt claims are serial and FIFO.
The shared worker now dispatches those commands into Antigravity, records input
and provider output locally, and publishes an ordered durable outbox. Reconnects
reuse event IDs; restart resumes the exact native session and leaves uncertain
commands interrupted. Codex also uses its native app-server protocol, with
explicit command/file/permission-profile approvals, streamed tool events, native
interrupts, and exact thread resume. Claude uses its native bidirectional
stream-json protocol with explicit permissions and exact session restart. Advanced Codex
question/form input and persistent policy edits report unsupported requests.
Conversation API contracts are recorded in `docs/go-parity.json`.
Cross-platform compilation does not establish process cleanup parity.
This is not yet a replacement for the full gateway. Non-loopback listeners
require `--tls-cert` and `--tls-key`; secure peer enrollment is still pending.
The Go server does not require a display, desktop keychain, or interactive login.
For a selected task, an Antigravity, Codex or Claude headless worker can run independently
of the desktop. Install/configure the native agent on that host and supply
the gateway credential through the environment:

```sh
./bin/amux worker run --state /path/to/worker.db --server https://gateway.example \
  --project project-ident --task TASK_UUID --directory /path/to/checkout \
  --prompt 'Inspect this task'
# Restart using the printed run ID and saved provider configuration:
./bin/amux worker run --state /path/to/worker.db --run-id RUN_UUID
```

Selected CLI and desktop-hosted runs now register their actual checkout and
advertise the current saved execution with a durable report sequence. They do
not opt into automatic assignment. Pool-managed runs keep their existing single
reporter. Source namespaces bind to the server-resolved API-key principal;
caller-supplied source names and user headers cannot establish that identity.
Older unbound rows remain observational until a fresh authenticated report.

Antigravity is the default provider. Use `--provider codex` or `--provider claude` for a new run;
restart preserves the saved provider. The native protocols are documented by
[Antigravity](https://antigravity.google/docs/cli/acp/) and
[Codex](https://developers.openai.com/codex/app-server).

Further prompts, permission choices, and cancellation use the conversation
command API. `--agent-command` accepts a JSON array to configure a native stdio
executable directly. This explicit conversation mode does not yet implement
full routing, project discovery, delegation,
or the remaining MCP catalog. Headless worker capability parity is still incomplete.

## Worker pools

Native worker pools advertise stable slots and consume ordinary and review assignments.
The server allocates eligible FIFO work on its 30-second tick; explicit native
busy state keeps an active slot unavailable. Each assignment atomically creates
one task conversation and initial prompt. Successful retries retain the run,
conversation, and initial prompt IDs. When the conversation has a recorded
principal, only the same enrolled client can replay the assignment. Legacy
conversations without a recorded principal can still be replayed by the exact
owner origin.

```sh
./bin/amux worker pool --state /path/to/pool.db --server https://gateway.example \
  --project project-ident --directory /path/to/checkout --provider codex --slots 2
# Restart with the printed pool ID and saved slot/native-session configuration:
./bin/amux worker pool --state /path/to/pool.db --pool-id POOL_UUID
```

After a slot releases a task, prompts in its task conversation can queue until
the original slot is idle. With a gateway that supports follow-up discovery,
the slot uses its saved run to resume the original provider session in the same
checkout for one turn. The reply leaves the task status as it was. If the pool
is stopped, use the restart command above with its original `--state` journal
and `--pool-id` so the slot can pick up the queued prompt. Follow-up discovery
only resumes task conversations created by the same enrolled client (the same
authenticated principal). Task conversations created before the gateway recorded
principals are not resumed automatically.

Pools share the same native engine as selected conversations. Turn completion
does not complete a task: its slot remains busy until the task workflow moves it
out of progress. Running native work survives transient gateway outages; uncertain
commands are retained for explicit recovery rather than repeated after restart.
Project turns prefer a queued review after ordinary work. Reviewer preferences
fall back when no better eligible peer is available; automatic fallback requires
a reported fresh-context capability. Native pools satisfy that by starting a
new conversation and provider session for each task. Use `--goal release` when
creating a pool or selected-task run to select a goal. Pool/run scope is saved,
advertised and retained on reconnect, including desktop-hosted saved runs;
`--pool-id` and `--run-id` reject goal overrides. Goal-tagged work requires an
exact match. Untagged tasks remain eligible for scoped pools. Native task MCP
list/create/claim calls carry the saved selection; a missing selection lists
the unscoped queue. Explicit session/delegated routing, project checkout discovery
and the rest of the Rust MCP catalog remain unfinished. This is partial scheduling
and headless capability parity. Presence and adoption API details are recorded
in `docs/go-parity.json`.

CLI workers now register an `amux` MCP stdio server with their native provider.
The standalone bundled catalog has 12 non-task tools. With a complete native
configuration and the bundled executables available, those join the 19 native
task and submission tools for 31 total. The native tools include the 15 server
tools, local `submit_work`, `agent_identity`, and the `task_agents` and `task_reassign` handoff tools.
`project_message_thread` reads the full original message and its replies after
inbox acknowledgement. The bundled tools use the pinned `agent-tools` and
`memory` executables. `AMUX_BUNDLED_DIR` is included in the runtime native
MCP environment allowlist. This establishes environment forwarding, not live
Codex provider acceptance.
The local `submit_work` tool runs Git on the worker checkout rather than on the
gateway server. Task tools also read/list/create, claim/comment,
release/park/complete tasks and answer the existing file-change confirmation
workflow. Tool mutations recheck
the current conversation instance inside the SQLite transaction. Credentials
remain in the inherited environment; the helper launch arguments contain the
credential variable name. `--no-task-tools` disables registration for one launch.
The transport supports the MCP lifecycle through revision `2025-11-25`; newer
protocol revisions and broader native approval policy parity remain incomplete.
Codex MCP registration defaults Amux tools to `approve` and disables Codex's
native update-plan tool only when the caller has not supplied an explicit
override for that key. Explicit user `-c` and `--config` values are preserved.
This does not change Codex's overall approval policy or sandbox settings.
Bundled catalog registration, MCP startup instructions and Codex launch
guidance are implemented; memory tombstone recovery and gateway push behavior
remain incomplete. See the
official [MCP stdio specification](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports).

MCP input frames are limited to 4 MiB. Ordinary trusted local tool results are
serialized in full for source-compatible output, without a 5 MiB cap.

The Fyne desktop can browse gateway projects, task status lanes, task details and
existing task conversations. The Board tab groups tasks by status and shows the
parent task on each subtask card. Project favorites and exact-first filters are
available for remote projects. Navigation refreshes through authenticated
WebSocket notifications, with reconnect resynchronization and coalesced updates.
Navigation preferences in the OS user configuration directory store device-local
favorites, filters and selection without the gateway credential. Use
`--preferences PATH` for another file or `--preferences=` for memory only.

The task details accordion includes a metadata editor with explicit Save/Cancel.
Drafts remain in memory per task across navigation and incoming detail updates;
only changed fields are submitted. Failed writes keep the draft and are not
automatically retried. Existing labels with embedded line breaks are preserved
when untouched; editing those labels requires another client.

Open **New task** to enter a title, specification and optional goal. After a
successful create, the submitted inputs are cleared (edits typed while the
request was pending are kept), the drawer closes, and a "Task <id> submitted."
notice appears below the task list. **Submit a new task** dismisses the notice
and reopens the drawer. An unconfirmed (unknown) create keeps its draft and
receipt across refreshes and offers **New task request**; it is never
automatically submitted again.

The task details accordion also contains a comment composer. Posting as a user
does not save or discard metadata edits. Typing during a pending post is kept,
and late replies cannot clear another task's draft. If the outcome is unknown,
**Refresh task before retrying** must successfully reload the task before another
post is allowed; the draft remains available for review. These composer drafts
and receipts are in memory and do not survive application restart.

The task details panel now offers **Close task**, **Send to user review** with a
specific review request, and **Return to queue** for parked work. The requested
decision is shown separately from any local draft. These controls act as the
user and preserve unsaved metadata and comment text. Parking and returning work
check the observed status atomically, so a concurrent close cannot be undone by
a stale request. Unconfirmed updates require an explicit refresh before retrying.
Cancelled tasks show their read-only report, including whether stopping the
executor was confirmed; viewing it never sends a cancellation request.

The **Native worker pool** panel starts workers for the selected project from an
existing local checkout. Choose Antigravity, Codex or Claude, 1–32 slots, and an
optional goal. The shared Go pool engine validates the actual checkout when it
registers with the server. **Resume saved pool** uses the saved pool ID and its
original gateway, project, checkout, provider, slot count and goal; edits to the
new-pool form do not replace that configuration. **Stop pool** preserves active
run journals for recovery, and closing the desktop cancels and joins its workers.
Use **Resume saved pool** with the original journal to let the original slot
pick up queued task conversation prompts. The same-client and pre-upgrade
limits of worker pool follow-up discovery described above apply.

Browser mode keeps its worker journal at `amux/workers.db` in the OS user
configuration directory. Use `--state PATH` for another journal or `--state=` to
disable local hosting. Only one process may own a journal at a time. Navigation
preferences remember the selected pool ID per gateway; credentials stay in the
environment. Worker task tools use the `amux` CLI beside the desktop executable
or on `PATH`, or the explicit `--task-tools-command` executable. Missing tools
and provider startup errors remain visible in the pool status.

## Desktop startup options

```sh
./bin/amux-desktop --server https://gateway.example \
  --preferences /path/to/navigation.json
# Optionally select an initial project:
./bin/amux-desktop --server https://gateway.example --project project-ident
# Open a conversation directly and optionally host its saved worker:
./bin/amux-desktop --server https://gateway.example --project project-ident \
  --conversation CONVERSATION_UUID
# Add --state /path/to/worker.db --run-id RUN_UUID to host the saved worker.
# Add --task-tools-command /absolute/path/to/bin/amux to enable its task tools.
```

### Tool approval rules

In desktop browser mode, a permission card offers **Always allow…** for local
chats and fleet conversations on this machine, when the request names a shell
command or tool and offers an `allow_once` option. Choosing **Always allow…**
opens an editable command pattern and directory, prefilled from the request or
conversation. For example, change `git log --oneline` to `git log *`, confirm
the directory, then choose **Save rule and allow**. This approves the pending
request and saves the pattern for future requests in that exact directory.
Draft edits survive conversation updates. A pattern must match the pending
request before it can be saved through this action.

Rules are stored under `directory_tool_approvals` in `work-preferences.json`
(the shared preferences file described above, overridden by desktop
`--work-preferences` or `AMUX_WORK_SUBMISSION_PREFERENCES`). Local chats and
native task workers read the file for each permission request, so new rules
apply without restarting the provider. The request's working directory takes
precedence over the conversation's directory. Existing `tool_approvals` string
rules remain global for compatibility. Remote fleet workers, archived task
transcripts, and direct `--conversation` windows do not offer local rule saving.

The two keys have this shape in `work-preferences.json`.

```json
{
  "directory_tool_approvals": [{"directory": "/home/me/project", "rules": ["git log *"]}],
  "tool_approvals": ["git status"]
}
```

When editing by hand, use an absolute `directory` path that exactly matches the
cleaned request directory. Omit trailing separators on non-root paths and match
letter case, including on Windows. Subdirectories do not inherit the rule.

In a rule an
unescaped `*` matches any text, `\*` and `\\` match a literal `*` and `\`, and
every other character is literal. The prefill escapes any `*` or `\` from the
request, so an unedited rule approves only that exact command. Requests
containing `; & | < > $` backticks or line breaks never match, so a rule cannot
approve a chained or redirected command. To revoke a rule, remove it from its
directory's `rules` list (or the legacy `tool_approvals` list) in the file.

Pending requests send an OS notification and remain visible in the browser's
approval banner and Events approval count until resolved. With one pending
request, **Open conversation** goes directly to the blocked conversation. With
several, **Review approvals** opens Events. The banner remains visible when OS
notifications are unavailable or suppressed. If the local client's approval
stream fails, the banner shows “Approval notifications unavailable. Reconnecting
to the local client…” while the desktop retries.

### Go desktop milestone check · 2026-11-22

These checks record implementation evidence, not release sign-off. Keep the
KISS/YAGNI/DRY review beside each milestone; the server, desktop and package
gates remain separate.

| Milestone | Check and evidence | Remaining gate |
| --- | --- | --- |
| Desktop gateway setup | **Implemented in the dev build.** Reuses local `GatewayPreferences` and existing pairing/storage flows. Settings cover local server/client mode, peer enrollment, remote-host advertisement and explicit restart. `desktop/gateway_settings_integration_test.go::TestGatewaySettingsEnrollmentPersistsAuthenticatingPeerAcrossRestart` verifies enrollment against a disposable mTLS gateway, preference persistence without storing the invitation key, authenticated access after gateway restart, and invitation expiry. | Verify startup-mode switching and remote hosting with separately packaged devices. |
| Task-board drag/reorder | **Implemented in the dev build.** Three board lanes use the transactional reorder endpoint; same-column acknowledgements update visible rank immediately. Review drops open the required review-action flow. `desktop/browser_workflow_integration_test.go` covers cross-lane, User Review and Done reorders; `desktop/task_display_order_test.go` checks rank display. A native devtest app confirmed Done→Done rank movement and the guided review drop. | Re-run the native interaction check on the final rebuilt package. |
| Re-cancelling a reopened task | **Implemented and verified.** A later cancellation replaces the prior report while keeping one database row. Evidence: `storage/task_cancellation_test.go::TestCancelAgainAfterCompleteAndReopenReplacesCancellation`; a live HTTP check returned the latest reason and one row. | This confirms report replacement; it does not establish that an external executor stopped. |
| Go release, upgrade and startup UI | **Partial.** Go bundles are packaged and checksummed; `appupdate/release.go` and `appupdate/stage.go` validate candidate assets and stage verified bytes. App controls include launch-at-sign-in and a portable-bundle update flow that verifies an update, downloads it, and offers explicit install/restart. On Linux amd64, a locally built Go candidate was overlaid on the published Rust 0.0.82 baseline; all checksums matched and the packaged GUI booted its gateway under Xvfb. The production CLI applied and relaunched a staged v0.0.83→v0.0.84 synthetic-version update, with all four replacement hashes matching. A fresh macOS arm64 package install/overlay passed. Isolated macOS updater tests passed for fixture A→B apply/relaunch and no-args relaunch. Windows amd64 executables were cross-built, bundled tools fetched, and the archive overlaid on a verified Rust 0.0.82 baseline with hashes checked. | Stable Go release publication and native Windows verification remain deferred follow-ups. |
| Conversational workspace/provider controls | **Implemented in the dev build.** Reuses workspace-scoped navigation preferences and persisted worker-run arguments; Claude exposes model/agent, Codex model, and ACP settings stay disabled where unsupported. View and conversation selection restore from preferences. Evidence: `desktop/conversation_launch_test.go`, `desktop/worker_manager_test.go::TestWorkerManagerPersistsConversationLaunchPreferencesInRunCommand`, and `desktop/browser_workflow_integration_test.go::TestBrowserRestoresConversationViewAndSelection`. A native macOS amux devtest restored the Conversations view, selected task conversation, and saved Claude model/persona without starting a real provider. | Live provider launch was not exercised in that devtest. |

**KISS/YAGNI/DRY check:** Gateway setup reuses the existing preference and
pairing paths; board interactions reuse Fyne lists and the atomic reorder API;
re-cancellation updates the existing report row; packaging reuses the verified
bundle format; provider preferences reuse workspace navigation storage and
saved run arguments. No additional feature framework or database schema is
needed for these milestones.

The full desktop package suite passed after these UI changes. The last
repository-wide suite result predates later changes and is not final acceptance.

