# Gateway handoff text

The shipped `amux` CLI accesses the gateway directly; agent-tools is not required.
Scratch text and Markdown documents travel inline in existing task specifications
and message bodies. There is no separate document store or binary attachment API
in this candidate.

Set `AMUX_GATEWAY_URL`, `AMUX_GATEWAY_PROJECT`, and `AMUX_GATEWAY_KEY` in the
environment. Keep the key out of command arguments. The URL must be an HTTP(S)
origin without a path prefix. The project value is the gateway project identifier.

```sh
amux messages list --limit 50 --offset 0
amux messages get 81
amux messages send 'Review context and findings, inline.' --subject 'Review handoff'
```

Listing includes resolved threads and supports pages of 1–500 entries. `get`
retrieves a complete thread using its positive numeric, gateway-local message ID.
Message IDs are not task UUIDs and must not be transferred between deployments.

Publishing requires an actual session launched by `amux agent codex` or
`amux agent claude`, in its original project directory. The same applies to agent
tabs launched by the desktop. The CLI checks the live registered process and
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
machine, platform or session target. Same-project and exact-session delegation
remain pending the gateway contract. An error or lost reply can leave accepted
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

`started`, `done` and `release` first read the task and continue only if it is
`in_progress` with this exact session and instance as owner. Otherwise no status
update or start report is sent, including for older claims that lack owner
session and instance. This read is a client check, not server authorization. The
gateway already guards claims with a bound owner origin, but until it enforces
the exact owner atomically for every claim type, ownership can change between
the read and the update.
Task cancellation is pending a gateway contract
that preserves the task and reporting actor; deleting a task is not cancellation.

Desktop reminder input currently requires explicit task selection and consent
with the supported Linux/Codex profile; see
[reminder validation](reminder-validation.md). Automatic routing and recovery,
Claude readiness and native Windows reminder input remain incomplete.

## Desktop task submission

The gateway sidebar accepts a task title and multiline handoff context for the
selected gateway project. It uses the same gateway client and acceptance checks
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
