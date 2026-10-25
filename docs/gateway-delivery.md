# Gateway task delivery

Set the gateway URL and API key in Preferences, then enable **Send gateway notifications and assign queued work to idle agents in this app** and save. No project identifier, machine UUID or machine key is required. The app creates a durable consumer identity and resolves repositories from Git remotes and gateway metadata.

Created tasks, comments and completed tasks produce a bounded `<gateway>` summary in a matching Claude or Codex terminal: task ID, title, event state, sender, available provenance, and the first 200 characters of the initial details or comment. Only the bounded preview is copied; complete descriptions and comment bodies are not injected. Preview text is normalized to one line and XML-escaped. Events older than five minutes and events already present when a connection is established are skipped. Freshness uses the gateway upgrade response’s HTTP Date plus monotonic elapsed time when available, so a local PC clock offset does not incorrectly expire a fresh event. Opening a later agent does not attach it to earlier notifications.

Each running agent has its own delivery session, pinned to its terminal, workspace and native process generation. Automatic delivery uses the owned terminal and process identity, with no screen parsing or prompt/footer matching. amux tees typed text and subsequent editing keystrokes into an in-memory buffer. Enter resets the buffer; modifier/navigation keys alone do not count as text.

A notification does not wait for an empty prompt or for a draft to be submitted. If user text is buffered, amux clears the provider draft, sends and submits the notification, then restores the buffered input in the same ordered terminal write. A native End key separates the message from Enter so a character-paste burst is flushed without a timer. Terminal protocol flags determine bracketed-paste encoding without reading rendered content. The current process, consent and event freshness are checked before the transaction. Each attempt is saved before enqueue; interrupted or uncertain attempts are never automatically repeated. Application restart retires pending notifications rather than replaying them.

Draft clearing uses End followed by Ctrl+U. Notifications never synthesize Ctrl+C,
which can interrupt a running turn or exit the provider when a draft tee is stale.

The task panel shows connection status, agents, pending notifications and recent receipts with the reason for each delivery decision. Gateway acknowledgment
confirms that the decision was saved, including a skip; it does not imply delivery.
`injected` means the notification input transaction was enqueued. It does not confirm provider understanding, task claim, execution or completion. Tasks remain authoritative on the gateway.

## Idle backlog review

Current queued tasks are checked separately from historical event replay. An
eligible local agent with no output for 30 seconds, no unfinished draft and
no owned in-progress task can take the first available queued item. The workspace
activity icons use this same output clock. Other owners, explicit recipients,
repository boundaries and task goals are respected; an agent can also evaluate a
queued task already assigned to it.

Unassigned TODO notifications remain visible in user event bubbles and wait for
this assignment path instead of being injected into competing agent terminals.
When several agents are idle, assignments rotate round robin within the task's
project, using each agent's own workspace and output clock. Only a confirmed
claim advances the rotation. Busy agents and unfinished drafts are skipped;
only the selected, confirmed owner receives the work prompt.

Polling follows each native agent's workspace, including workspaces whose initial
project lookup failed. Resolving or refreshing one workspace does not prevent a
different workspace with current tasks from handing off queued work.
Gateway list summaries omit `project_ident`; the client binds missing project
fields to the requested workspace project. Explicit foreign projects are rejected,
and a fresh task-detail read still confirms the project before a claim.

The app rereads and claims the selected task through the gateway and posts a
comment asking the agent to check whether it remains required, ask the user if
uncertain, close it with a reason if obsolete, or start execution otherwise.
It then rechecks the owner and native session before sending anything to the
terminal, and requests evaluation against current project state: implement viable work,
review uncertain work with the user, or close obsolete work with a reason. The
app does not decide viability or close tasks itself. Only one task is handed to
an idle session at a time; another idle peer can take the next item.

Claims and input attempts are saved before sending. Unknown outcomes pause the
affected handoff for inspection rather than retrying automatically. Restart does
not replay input. A newer explicit requeue retires a known handoff fence;
unconfirmed attempts remain paused until the task is resolved. Completed tasks
retire their old backlog fences. This does not replay old lifecycle events.

OS-restricted tasks use one explicit `os:windows`, `os:linux` or `os:macos`
label. Pickup and notification delivery require a matching verified native OS;
unavailable platforms leave their work queued. Retagging cancels incompatible
pending input without replaying uncertain attempts. See the project-agent and
OS-routing commands in [gateway-cli.md](gateway-cli.md#project-agents-and-os-routing).

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

Parked tasks never enter idle assignment, in-progress reminders or terminal
notification delivery. Review events remain visible in user event bubbles.
Authoritative review snapshots also retire older queued notifications and
handoffs; input already in flight retains an uncertain outcome. When the user
returns work to TODO, normal eligibility resumes.

## Autoreview

Enable **Autoreview** in Gateway preferences to review committed code when a
verified local agent has had no output for 30 seconds and the project's queued
or TODO list is empty. This setting is off by default and is independent of
ordinary gateway notifications. Pending task actions, owned work, unfinished
input, stale task lists and uncertain deliveries prevent a review from starting.

Each review covers at most five of the newest unreviewed commits. When available,
amux prefers a different provider from the latest commit's author/coauthor or
the previous reviewer. Eligible tabs in the same local checkout can be selected
across workspaces. The agent reads project coding guidelines first; if none exist,
`amux --get-guidelines` prints the bundled general guidelines.

The injected instruction permits review and finding-task creation only, not code
changes, commits, pushes or PRs. Findings use `amux tasks create --review-finding`
and the `autoreview-finding` label. Unassigned findings stay available for human
triage and are excluded from automatic notification injection and task pickup;
explicit assignment permits normal delivery.

Review receipts are saved per checkout on this device before terminal input.
Commits are marked reviewed only when the assigned agent runs the supplied
`amux --agent <id> reviews complete <review-id>` command after completing the
review and confirming any finding tasks. Sending input alone is not completion.
`amux reviews status` shows the receipt. Interrupted or uncertain reviews are not
automatically replayed; after confirming the old review has stopped, an operator
can run `amux reviews release <review-id>` to permit another attempt. Different
devices keep independent review histories.

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
or actor announcement is required. Notifications are filtered per native agent: assigned task creation, comments,
and completion go only to the owner; unassigned TODO work waits for assignment to
an eligible idle agent. The exact `owner_origin` takes precedence. When a legacy snapshot
only has `owner_agent_id`, it must match the verified agent's actual gateway ID.
An unknown owner ID is never treated as unassigned. An exact origin suppresses
its own notification, including an agent's own comments. Broadcast routing cannot
override a current owner. Explicit queue-only creation still sends no notification. Named-goal events still require an
explicit matching goal and are skipped when no goal is bound.

The existing credential-bound `amux dispatch` CLI is a separate gateway protocol
and retains its documented administrator-issued machine credentials. Those
credentials are not used by desktop lifecycle notifications.

## Verification

Contract tests cover local WebSocket subscription/reconnect, offline-tail suppression, routing, durable attempts and acknowledgments, and built-in identity. Controlled native terminal fixtures exercise exact input, immediate delivery with restored user drafts, text-versus-navigation tracking, hidden footers, unrecognized layouts, stale callbacks and three-peer isolation. Native Windows tests run on Windows CI; installed Windows Codex submission still needs confirmation on the user's machine. Controlled fixture input is not evidence of provider understanding or task execution.


The left-side **Gateway messages** drawer presents sender bubbles with task
title, preview, project, state at the time of the event, age, and task ID. Hover
for delivery details, which remain separate from task state. The latest 50
previews are saved independently of delivery receipt pruning. Clear hides the
view without deleting delivery fences or history. Historical snapshots do not
claim to be the task's current state, and old journal entries without summaries
show explicit unavailable fields rather than forcing a lookup or inventing text.

Delegated-task creation uses the gateway's requester name, requester hostname,
and source project. Comments use their own author; a task's recorded hostname
is labeled **Recorded task host**, because it may differ from the commenter's
machine. Missing sender-machine metadata is shown as unavailable. The recipient's
assigned amux identity remains separate from the sender and is included for task
mutations; fetching full task details is optional when the preview suffices.
