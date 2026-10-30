# Gateway handoff text

The shipped `amux` CLI accesses the gateway directly; agent-tools is not required.
Scratch text and Markdown documents travel inline in existing task specifications
and message bodies. There is no separate document store or binary attachment API
in this candidate.

`amux tasks` uses the enrolled native Server or Client connection saved in Settings
and resolves the project from the checkout's Git origin. `--project` or
`AMUX_GATEWAY_PROJECT` can select the project. `--gateway-url` can verify the saved
endpoint but cannot select a different authority; pair that server in Settings first.
The retired `AMUX_GATEWAY_URL`/`AMUX_GATEWAY_KEY` connection fallback is removed.

For the message commands below, set `AMUX_GATEWAY_PROJECT` to the native project
identifier. Connection credentials come from the enrolled profile.

```sh
amux messages list --limit 50 --offset 0
amux messages get 81
amux messages send 'Review context and findings, inline.' --subject 'Review handoff'
```

Listing includes resolved threads and supports pages of 1–500 entries. `get`
retrieves a complete thread using its positive numeric, gateway-local message ID.
Message IDs are not task UUIDs and must not be transferred between deployments.

Publishing requires an actual registered agent in its original checkout.
Use `amux agent codex`, `amux agent claude`, or a provider started in a desktop
terminal. amux derives the checkout UUID from Git upstream and canonical directory,
and names agents by provider and terminal number, such as `codex-1`. The CLI checks the live registered process and
calling ancestry before sending its session/instance headers. Reads do not
require an agent launch. The gateway currently stores the sending agent ID on
messages; these headers alone do not establish authenticated machine binding or
full session provenance on the server.

`send` may notify the project's configured external channel. A successful reply
must contain a positive canonical message ID. A failed or lost reply leaves
publication uncertain: inspect the thread list before manually retrying. The
client does not retry publication or promise request deduplication. Encoded
inline messages are limited to 1 MiB and responses to 4 MiB.

Use `amux tasks --help` for the existing task operations. Creation succeeds only
when the reply identifies a canonical, non-nil task UUID in the requested project
with a known task status. Empty, malformed or mismatched replies leave acceptance
uncertain: fetch tasks before retrying. The client does not retry creation or
establish server request deduplication.

`amux tasks delegate 'Review implementation' --target-project other-project
--specification 'Inline handoff context'` uses the existing cross-project
delegation route. Run it from an amux-launched agent in its original project,
with `AMUX_GATEWAY_PROJECT` identifying the source project. The result must
identify canonical source and destination task UUIDs and a delegation record
linking those exact tasks and projects. Titles are limited to 1 KiB without
control characters; context is limited to 64 KiB of UTF-8 and stays literal.

This creates shared tasks, not a claim, execution start, destination receipt or
exact-session assignment. The endpoint requires different projects and has no
machine, platform or session target. Use ordinary-task reassignment below for
a connected agent in this project. An error or lost reply can leave accepted
tasks behind: inspect both projects before manually retrying. The client makes
one request and does not establish server deduplication. The gateway may notify
the destination project's configured channel.

A claim must acknowledge the requested task UUID and project as well as the exact
session and instance as owner. Claim acknowledgment does not report execution
start. Task reads and updates also require the requested task and project;
updates must acknowledge the requested status. Comments require a canonical
comment UUID attached to the requested task, including explicit start reports
which are currently ordinary comments. Ambiguous replies leave the operation
unconfirmed and are not retried.

`started`, `done` and `release` first read the task (without its comments, so
long threads stay under the response cap) and continue only if it is
`in_progress` with this exact session and instance as owner. Otherwise no status
update or start report is sent, including for older claims that lack owner
session and instance. This read is a client check, not server authorization. The
gateway already guards claims with a bound owner origin, but until it enforces
the exact owner atomically for every claim type, ownership can change between
the read and the update.

## Replacing `agent-tools tasks`

The published `agent-tools 1.22.0` task workflows map to the commands below, with
explicit identity and retired-Eventic exceptions. Mutations always use
the caller's amux identity (from the agent terminal's environment, or an explicit
`--agent <id>` for headless use), never a copied or overridden agent ID.

| agent-tools | amux |
| --- | --- |
| `tasks list [--status S] [--include-stale]` | `amux tasks list [--status S] [--include-stale]`; statuses are `todo`, `in_progress`, `user_review`, `done`, `cancelled` |
| `tasks get ID` | `amux tasks get ID [--comments N]`; shows the newest 50 comments by default, numbered within the full thread (`--json` for raw gateway JSON) |
| `tasks add --title T [--description D] [--specification S] [--label L]… [--hostname H]` | `amux tasks create T [--description D] [--specification S] [--label L]… [--hostname H]`; use `--os` and `--review-finding` instead of `os:` or `autoreview-finding` labels |
| `tasks add-delegated --target-project P --title T --description D --specification S [--label L]… [--hostname H]` | `amux tasks delegate T --target-project P [--description D] [--specification S] [--label L]… [--hostname H]` |
| `tasks claim ID` | `amux tasks claim ID` (also refuses tasks that require another OS) |
| `tasks release ID` | `amux tasks release ID` (exact owner only) |
| `tasks done ID` | `amux tasks done ID` (exact owner only) |
| `tasks comment ID TEXT` | `amux tasks comment ID TEXT` |
| `tasks rank ID N` | `amux tasks rank ID N`; the reply must acknowledge the new rank |
| `tasks help [COMMAND]`, `--help` | `amux tasks help [COMMAND]`, `--help` |
| `--details S` | Alias for `--specification S` on `create` and `delegate` |
| `--agent-id`, `--reporter` | Not supported: the reporter and author are always the assigned identity |
| `tasks comment --author-type user` | Not supported: agents cannot post as the user |
| `tasks builds` | Not ported; Eventic build status is deprecated |

`--hostname` defaults to this machine's hostname, as in agent-tools. amux also
provides `agents`, `reassign`, `set-os`, `user-review`, `started`, `cancel`,
`cancellation` and `bind`, described below.

Use the full task UUID printed by amux; identity overrides are replaced by the
caller's amux identity. Native human actions remain available in
the task pane. Goal/profile work present only in the agent-tools working tree at
the audit is not part of this released baseline. The deprecation handoff must
reconcile any such work before switching generated rules; it must not remove a
newly released workflow without a replacement.

## Project agents and OS routing

`amux agents` lists local registrations. For the current project's connected
agents across machines, including name, host, provider, OS and exact execution
identity, use:

```sh
amux tasks agents
amux tasks agents --os windows
amux tasks reassign <task-id> --to-agent <recipient-session-uuid>
amux tasks create 'Verify Windows behavior' --os windows
amux tasks set-os <owned-task-id> --os windows
amux tasks set-os <owned-task-id> --clear
```

Discovery verifies the checkout's project/repository and excludes expired
entries. Reassignment resolves one fresh compatible execution and sends the
calling agent's identity, never the recipient's credentials. Ambiguous, stale
or incompatible recipients fail without a fallback. After a confirmed handoff,
stop working on that task. Unknown replies require inspection before retrying.

OS requirements use one explicit label: `os:windows`, `os:linux` or `os:macos`.
No OS label means unrestricted; titles such as “WINDOWS ONLY” are not parsed.
Other labels are preserved. `set-os` requires the exact owner of active work;
when the new OS excludes that owner, the gateway atomically releases the task
to the queue and clears stale session routing. If no compatible agent is
available, it remains queued. User review and closed tasks cannot be handed off.

Enforced OS creation, claim, retagging and reassignment require the gateway OS
contract available in gateway v1.28.0 and later. Older gateways
cannot enforce the requirement for all clients; an unconfirmed ownership release
is an error, never permission to retry or silently fall back to another OS.
The amux server also filters notifications, assignment, reminders and human
assignment by the OS requirement. Work for a different OS does not block an
eligible agent.

### Requesting task results

For a same-project handoff, create a task for a live agent and subscribe the
calling execution to its comment and completion updates:

```sh
amux tasks create 'Review the design' --to-agent <recipient-session-uuid> --specification 'Review only; report findings.'
amux tasks create 'Track this work' --notify-results
amux tasks watch <task-id>
amux tasks watch <task-id> --clear
amux tasks delegate 'Review another project' --target-project other-project --notify-results
```

`--to-agent` enables return notices automatically. `--notify-results` adds a
subscription without selecting an executor. `watch` manages a subscription only
where the gateway authorizes this caller (the current owner or existing requester).
The subscription records the caller's exact advertised origin and execution
generation, independently of task ownership, and survives executor reassignment.
No display-name or reporter lookup is used as a return address.

The native task detail panel provides **Result recipient…** and **Clear result
recipient** for explicit human selection. Human creation and assignment default
to no requester and do not impersonate a selected agent. Task detail output shows
the stored result recipient; these operations never claim or reopen work.

These commands require the gateway capability endpoint
`GET /v1/tasks/capabilities` to advertise `requester_subscriptions: 1`, plus
`PUT /v1/projects/{ident}/tasks/{id}/requester`. Older gateways are rejected before
the subscription mutation. Creation and update responses must acknowledge the
exact requester; an unconfirmed response requires inspection before retrying.
The gateway dependency is tracked in task `01a1796a-bc87-7c40-bafa-17b83a919a89`,
implemented at source revision `986d84544aae086579c72349a1afc366cef6b589`.
A minimum deployed release has not yet been verified; do not assume an existing
release supports this without checking the capability.

See [result delivery](gateway-delivery.md#requester-result-notices) for idle/input,
freshness and reconnect behavior. Cross-project linked delegation remains a separate
protocol: `delegate --notify-results` explicitly subscribes the caller to its source
tracking task, not the foreign-project execution. It requires the gateway to mirror
the target result to the source; no cross-project watcher is inferred.

## Task cancellation reports

```sh
amux tasks cancel TASK_UUID --reason 'The user withdrew this task.'
amux tasks cancellation TASK_UUID
```

Cancellation requires the gateway's explicit `POST /tasks/{id}/cancellation`
contract. It records `cancelled` while retaining the task, prior claimant,
reporting session and history. The CLI verifies the exact task/project, reason,
reporter and preserved owner before acknowledging the report. This operation
requires an amux-launched caller in its original project. The server must enforce
ownership atomically; the shared bearer and origin headers alone do not
authenticate a machine.

The report explicitly has `executor_stop_confirmed: false`. Recording it does
not stop an executor or establish that external work stopped. Dispatch-managed
tasks use the separate credential-bound dispatch protocol; ordinary cancellation
must reject them. An unavailable endpoint fails without falling back to deletion
or completion. Deployment of the owner project's reviewed cancellation revision
is required; local client fixtures do not establish deployment.

After an error or lost reply, use the read-only `cancellation` command to retrieve
the durable report before taking further action. The client sends no automatic
retry. Reasons are trimmed and limited to 16 KiB of UTF-8; responses remain
limited to 4 MiB. Reading a report does not claim the task and needs no agent
identity.

Enable fresh automatic notifications in desktop Gateway preferences; see
[gateway delivery](gateway-delivery.md) for routing, readiness and input checks.
The older selected-task reminder path is described in
[reminder validation](reminder-validation.md).

## Desktop task submission

The gateway sidebar accepts a task title and multiline handoff context for the
automatically resolved workspace project. The desktop matches the workspace’s
Git origin to the gateway’s canonical repository mapping, including aliases;
the enrolled connection is configured in **Settings → Gateway Server**.
It uses the same gateway client and acceptance checks
as the CLI. This is a desktop user action, not an agent execution: it sends no
agent session headers and does not claim or start the task.

The form distinguishes an unsent draft, a request awaiting acceptance, a
confirmed task UUID, and an unknown result. The receipt keeps its original
project and task UUID even if the user changes projects or workspaces before the
reply. Refreshing the task list does not overwrite that receipt or retry a POST.

After an unknown result, inspect gateway tasks before choosing **Start different
request**. That action opens an empty form; it does not retry the previous
request or delete a gateway task. Drafts and the current receipt are temporary
and are not restored after exit. Accepted tasks remain in the gateway. Titles
are limited to 1 KiB of UTF-8 and context to 64 KiB; the gateway may impose further
validation.

The desktop state tests and a native Linux GUI/local HTTP fixture cover late
replies across project changes, duplicate clicks, literal multiline context and
an uncertain response without automatic retry. Native Windows interactive task
submission acceptance remains pending platform review.

For routing work to one exact running agent with credential-bound sessions, see
[exact-session dispatch](dispatch-cli.md).
