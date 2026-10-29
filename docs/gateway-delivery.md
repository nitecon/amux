# Gateway task delivery

Automatic task work runs on the amux server (embedded **Server** mode, or
**Client** mode against a dedicated amux server) through its event router; see
[StateMachine.md](StateMachine.md). Enable **Let the amux server assign queued
work and send task notifications to agents in this app** in Settings. A legacy
gateway has no event router: the desktop then does no automatic assignment,
review or terminal notification, and you ask an agent directly.

The desktop connects to `/v1/events/stream`, reports each running agent
(`client.heartbeat`) and its terminal activity (`terminal.activity`: last output
time on the server's clock, and whether a typed draft is present), and delivers
`agent.notify` events into the named agent's terminal. The server decides who
hears about what; the desktop decides only whether it can deliver safely now,
and reports `delivered`, `skipped` or `failed` back as `agent.notify_result`.

Notifications are a bounded `<gateway>` summary: task ID, title, event state,
sender, available provenance, and the first 200 characters of the initial details
or comment, normalized to one line and XML-escaped. Notifications older than five
minutes are skipped.

Each running agent is pinned to its terminal, workspace and native process
generation. Delivery uses the owned terminal and process identity, with no screen
parsing or prompt/footer matching. amux tees typed text and subsequent editing
keystrokes into an in-memory buffer; Enter resets it and navigation keys alone do
not count as text. A notification clears a buffered draft with End and Ctrl+U,
sends and submits the notification, then restores the draft in the same ordered
terminal write. A native End key separates the message from Enter so a
character-paste burst is flushed without a timer. Terminal protocol flags decide
bracketed-paste encoding. Notifications never synthesize Ctrl+C. The agent's
process is verified natively immediately before writing; a changed process is
reported `failed` without input.

The Events panel shows the router connection and recent notifications with their
outcome. `delivered` means the input transaction was enqueued; it does not confirm
provider understanding, task claim, execution or completion. Tasks remain
authoritative on the server.

## Requester result notices

An explicit task requester receives comment/completion notices even when another
agent owns the work. The requester is a server-stored exact native origin plus
execution generation; it is never inferred from the reporter, display name or
comment author. Project, OS, goal and own-event suppression still apply.

These notices are informational, with a read-only task lookup command. They do
not claim, reopen or schedule execution. They wait on the desktop until the
requester has had no terminal output for 30 seconds and has no draft, within the
five-minute lifetime. See [CLI and native controls](gateway-cli.md#requesting-task-results).

## Queued work assignment

The server's `assignment` plugin gives each idle agent (no terminal output for 30
seconds, no draft, no owned in-progress task) the oldest eligible queued task in
its project. Creation time defines FIFO. Eligibility respects the task's OS label,
review exclusion, explicit User review, goal, delegation, owner and exact session
routing. The server claims the task as that agent through the normal task store,
comments why, and sends a `Queued task review` (or `Queued independent review`)
notification asking the agent to check whether the task is still required,
implement it, ask the user, or close it with a reason. One task is handed to an
idle agent at a time.

If the desktop cannot deliver the offer (the agent became busy, started typing,
or its process changed), it reports the outcome. The server releases an untouched
offer only for its captured reporting source and target; a delayed result cannot
undo subsequent progress or another claim. The next idle agent can then receive
the task. When an in-progress task has had no update
or comment for ten minutes, its connected owner receives `Task update requested`
(again every ten minutes while it stays quiet); a progress comment resets the wait.
Claimed work returns to the queue after 15 minutes without task activity unless
the server sees its owner busy.

After the desktop restarts, the server also reminds resumed sessions about
their remaining in-progress tasks. A fresh execution receives an explicit
prior-execution notice without acquiring ownership. These informational
reminders wait for idle; reconnecting the same desktop does not produce another
restart reminder. Supporting servers advertise `server_restart_reminders` at
subscription. An older event router without this flag uses the retained local
restart journal instead; a legacy gateway returning 404 enables no fallback.
The reminder plugin switch gates periodic nudges, while startup continuity
remains available.

OS-restricted tasks use one explicit `os:windows`, `os:linux` or `os:macos`
label; only agents on that OS receive them. See the project-agent and OS-routing
commands in [gateway-cli.md](gateway-cli.md#project-agents-and-os-routing).

## User review

Tasks awaiting a human decision use the gateway's `user_review` status (gateway
v1.27.0 or newer). An agent can record its question in a task comment and run
`amux --agent <assigned-id> tasks user-review <task-id>`; `review` is an alias.
Only the exact owning agent can request this transition. The acknowledgment must
confirm that ownership and execution timestamps were cleared. An uncertain
response requires inspection before any retry.

The task panel has Active (the default), User review and Completed filters.
Users can inspect parked tasks and their comments, return them to the queue, or
close them. Returning a task uses the human connection without agent headers.
Agents cannot resume parked work themselves.

Parked tasks never enter assignment, in-progress reminders or owner
notifications. An explicit requester can still receive an informational comment
result. When the user returns work to TODO, normal eligibility resumes.

## Completion reviews

When a task completes, the server's `review` plugin queues one ordinary review
task for a different agent (the completing agent and every agent that commented
on the task are excluded, including by the task store's claim and assignment
checks). Reviews use the same FIFO assignment
as other work. The server creates at most one review per source completion, and
completing a review does not create another review.

Reviews inspect the source task, its comments and actual changes. They follow
KISS/YAGNI, create tasks only for concrete actionable defects, and avoid edits,
commits, pushes and speculative scope expansion. Results are posted on the source
and review tasks, then the review task is marked done.

A TODO finding is executable queued work; its `autoreview-finding` label is
descriptive. Only explicit **User review** waits for a human decision.

Server plugins can be switched off with `PUT /v1/plugins/{id}`
(`assignment`, `notifier`, `reminder`, `review`, `reclaim`); `GET /v1/plugins`
lists them.

`GET /v1/presence/summary` returns live presence across the gateway:
`{"machines", "os", "agents", "active"}`, where machines are distinct authenticated
reporting sources with a fresh heartbeat, including machines reporting no agents.
OS names are a set, so two Windows machines still count separately. Native
agent observations count before project resolution; task assignment and startup
reminders wait for a nonempty project. Active agents are observed busy agents.
Heartbeats include optional host OS metadata even with an empty inventory.
Connections sharing the bearer principal share one reporting source. The desktop status bar polls it every
30 seconds for its first line; the second line counts this machine's agents.

`PUT /v1/checkouts` replaces one machine's checked-out projects:
`{"hostname", "checkouts": [{"project", "origin"}]}`, where `origin` is the
workspace's Git origin URL without HTTP(S) credentials. Each machine's last report
is kept, so offline machines still show where a project lives. `GET /v1/checkouts`
lists every project with `{"ident", "canonical_remote", "machines": [{"hostname",
"origin"}], "agents", "active"}`. Every 30 seconds the desktop reports its open
workspaces and lists the other projects in the sidebar: those checked out on other
machines first, then the rest, each alphabetically. Right-click one to clone it from
a reported origin into the Default Projects directory or a chosen folder. This
data is display only and never affects routing.

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

Amux advertises each session's machine automatically. Notifications snapshot the
sender and owner hostnames from these registrations; agents need no hostname
argument or reporting step. Server-generated offers identify amux and the server's
machine. Completion reviews record their creation on the server. The task's
recorded creation host stays separate from its owner's machine. Remote owner
labels use the latest live registration for the session after a restart; task
mutation and delivery authorization still require the exact execution identity.

Use the bundled `amux agents` command to inspect local running peers. The desktop
makes the bundled CLI available on terminal PATH.
Gateway task commands use the saved gateway preferences and resolve the project
automatically; environment variables and flags can override those settings. amux
owns assignment and registration. No agent-tools update, provider hook,
or actor announcement is required. Notifications are filtered per native agent: assigned task creation, comments,
and completion go only to the owner; unassigned TODO work waits for assignment to
an eligible idle agent. The exact `owner_origin` takes precedence. When a legacy snapshot
only has `owner_agent_id`, it must match the verified agent's actual gateway ID.
An unknown owner ID is never treated as unassigned. An exact origin suppresses
its own notification, including an agent's own comments. An explicit requester
subscription separately permits informational comment/completion returns to the
exact requesting execution, as described above. Broadcast routing cannot
override a current owner. Explicit queue-only creation still sends no notification. Named-goal events still require an
explicit matching goal and are skipped when no goal is bound.

The existing credential-bound `amux dispatch` CLI is a separate gateway protocol
and retains its documented administrator-issued machine credentials. Those
credentials are not used by desktop lifecycle notifications.

## Verification

The event client uses bounded connection attempts, a 60-second stream silence
deadline and five-second reconnect retries, including after sleep or a VPN route
change; a gateway without the event router is rechecked every five minutes. On
reconnect the server resends any notification that was not acknowledged.
Connection loss and recovery produce deduplicated, dismissible corner
notifications without resizing the terminal.

Real-server contract tests (`tests/event_stream_contract.rs`) cover subscription
by event and target, acknowledgment and resend, cursor resume, remote emits,
presence, FIFO assignment and release, owner notices, completion reviews and
plugin switches. Controlled native terminal fixtures exercise exact input,
bracketed paste, immediate delivery with restored user drafts, idle-gated offers,
failed verification and peer isolation. Controlled fixture input is not evidence
of provider understanding or task execution.

The left-side **Gateway messages** drawer presents sender bubbles with task
title, preview, project, state at the time of the event, age, and task ID. Hover
for the delivery outcome, which remains separate from task state. Historical
snapshots do not claim to be the task's current state.

Delegated-task creation uses the gateway's requester name, requester hostname,
and source project. Comments use their own author; a task's recorded hostname
is labeled **Recorded task host**, because it may differ from the commenter's
machine. Missing sender-machine metadata is shown as unavailable. The recipient's
assigned amux identity remains separate from the sender and is included for task
mutations; fetching full task details is optional when the preview suffices.
