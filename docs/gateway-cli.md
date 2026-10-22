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
