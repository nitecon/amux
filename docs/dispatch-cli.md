# Exact-session dispatch

`amux dispatch` uses the gateway's credential-bound session dispatch API
(agent-gateway `docs/session-task-dispatch.md`). It routes work to one exact
running agent, including one of several sessions of the same provider in the
same project, without agent-tools or provider hooks. It does not provision
credentials, deploy the gateway, launch executors or deliver prompts.

## Identity and credentials

| Gateway field | amux value |
| --- | --- |
| `origin.session_id` | The amux wrapper session UUID (the declared execution session) |
| `origin.instance_id` | The provisioned machine UUID (`AMUX_GATEWAY_MACHINE_ID`) |
| `generation` | The amux wrapper instance UUID, new for every launch |
| `provider`, `os` | The launch's provider and native OS |

The wrapper session is amux's execution identity, not a provider-native
conversation ID. A gateway operator issues the machine credential with
`gateway task-machine issue`. Supply it as `AMUX_GATEWAY_MACHINE_ID` and
`AMUX_GATEWAY_MACHINE_KEY`, alongside `AMUX_GATEWAY_URL`, `AMUX_GATEWAY_PROJECT`
and `AMUX_GATEWAY_KEY`; amux never writes the machine key to disk.

Each generation gets its own session secret: 32 bytes from the OS random source,
stored hex-encoded under the amux state directory in `dispatch/keys/` with
`0600` permissions, before its first registration. These credentials prove an
authorized machine and generation to the gateway. They are not hardware or
provider attestation.

Every command must run inside an `amux agent codex|claude` launch, from its
project directory. Callers that are not verified amux launches are refused
before any secret is created or any request is sent.

## Commands

```sh
amux dispatch register                      # register or heartbeat (lease: 120 s)
amux dispatch sessions                      # live eligible targets in this project
amux dispatch delegate "Title" --target '<target JSON>' \
    [--target-project other] [--specification text] \
    [--require-os linux] [--require-provider codex]
amux dispatch delegate "Title" --machine-target <machine-uuid> \
    [--require-os windows] [--require-provider codex]
amux dispatch act <task> receive
amux dispatch act <task> claim
amux dispatch act <task> start --claim-id <id> --execution-id <uuid>
amux dispatch act <task> succeeded --claim-id <id> --execution-id <uuid> --result "outcome"
amux dispatch act <task> request_cancel --claim-id <id> --result "reason"
amux dispatch act <task> cancel [--claim-id <id> --execution-id <uuid>] --result "reason"
amux dispatch act <task> handoff [--claim-id <id>] --target '<target JSON>' --result "reason"
amux dispatch act <task> handoff [--claim-id <id>] --machine-target <machine-uuid> --result "reason"
amux dispatch recover                       # read-only recovery for this generation
amux dispatch requests                      # requests with unknown outcomes
amux dispatch replay <request-id>           # resend one, unchanged
```

Targets are the JSON objects printed by `sessions`. Registration must be
heartbeated before the 120-second lease expires; amux does not run a background
heartbeat. Claiming never reports a start: `start` is a separate, explicit
report by the executor after it has launched, carrying its own execution UUID.
`request_cancel` records a request; it does not stop an executor. `cancel` of
running work requires the executor's execution UUID as its confirmation that it
stopped.

## Offline machines

`--machine-target` queues work for a provisioned machine instead of one live
session, so it can be delegated or handed off while the destination is off
(agent-gateway `1b3f4b4`). It and `--target` are mutually exclusive. The gateway
takes the machine's OS from its provisioning record; `--require-os` and
`--require-provider` stay exact constraints. Accepted work is `pending_machine`
with no bound session, claim, receipt or start.

When a session on that machine starts, `recover` lists the work in
`machine_pending`; reading it changes nothing. The session takes an item only
with an explicit `act <task> receive`, which binds it to that exact session and
generation and leaves the task TODO and unowned. A competing session's receipt
is rejected. Claim and start remain separate explicit reports, and once bound
all exact-session fences apply. A later generation cannot receive work already
bound to an earlier one. Exact-session requests are unchanged: they carry no
`machine_target` field, so their stored bodies and replay fingerprints stay the
same.

## Requests and recovery

Every `delegate` and `act` request is written to the dispatch journal
(`dispatch/journal.json`) with a fresh request ID before it is sent. Every amux
process on the machine shares the journal: each read or write takes its lock
only for one short read-modify-write (waiting at most 2 seconds), never across a
network call, so one agent's slow request does not block another's. It is
limited to 512 requests and 2 MiB, and written through a synced private
temporary file and a directory sync. When a new write would exceed either limit,
the oldest `accepted` or `rejected` requests are dropped under the same lock;
`uncertain` requests are never dropped, so a journal full of them refuses the
write. A dropped request ID is unknown to `replay`, which sends nothing. If it
cannot be written, nothing is sent. A corrupt, oversized
or unknown-version journal is preserved and refused, never reset or skipped.

A request becomes `accepted` when the gateway acknowledges exactly what was
asked, or `rejected` on a definite 4xx rejection. A lost reply, a 5xx or a
malformed acknowledgment leaves it `uncertain`: the gateway may have committed
it. If recording the outcome fails, the journal still shows it `uncertain`.
amux never resubmits on its own.

`replay` resends an uncertain request byte for byte, with the same request ID,
so the gateway returns its original response instead of acting twice. Before
sending, it rechecks the stored request's shape (request ID, generation, path
and operation) and that the same gateway, project, machine, session and
generation are making the call. A later launch, even of the same agent, has a
new generation and cannot adopt or resend an earlier generation's requests or
claims; use `recover` to inspect their state instead.

## Acknowledgment checks

A reply is accepted only if all of these hold, otherwise the request stays
uncertain and the command fails:

- the task UUID is not nil and matches the dispatch, and the task belongs to the
  dispatch's project;
- `authentication` names this exact session and generation, and its machine
  record has this project, machine UUID, OS and credential ID (the SHA-256 of
  the machine key);
- delegation: the exact requested target (state `pending`) or the requested
  machine with no bound session (state `pending_machine`, and its OS matching
  `--require-os` when given), destination and source projects, no receipt,
  task `todo`, nothing claimed;
- `receive`: state `received` with `received_at`, bound to this session (and,
  for machine work, addressed to this machine), task `todo`, nothing claimed;
- `claim`: state `claimed` with `claimed_at`, a claim ID and no start time, task
  `in_progress` owned by this origin;
- `start`: state `running` with `started_at`, echoing the claim and execution
  IDs;
- `succeeded` / `failed`: that state with `finished_at`, echoing claim,
  execution and result, task `done`;
- `request_cancel`: claimed or running with `cancel_requested_at` and the
  reason, task still `in_progress`;
- `cancel`: state `cancelled` with `finished_at` and the reason, task
  `cancelled` (agent-gateway `f060414`);
- `handoff`: state `pending` for the requested new target, or `pending_machine`
  for the requested machine, with no receipt, task `todo`, nothing claimed.

"Nothing claimed" means no claim ID, claim, start, execution, finish or
cancellation request on the dispatch, and no owner, start or done time on the
task. Residue from earlier work makes the acknowledgment malformed, so it is
never recorded as accepted.

Inputs are checked before anything is stored: no nil task or execution IDs, no
blank claim IDs, and results limited to 64 KiB. Responses are limited to 4 MiB
and the session list to 1,000 entries. Recovery is refused if its pending or
requested work names another generation, or if `machine_pending` holds work
that is bound or addressed to another machine or OS.

## Limits

- Offline-machine delivery needs agent-gateway `1b3f4b4` or later; an older
  gateway rejects `machine_target` and amux reports the rejection.
- Not run against a deployed gateway or real provisioned credentials; evidence is
  local HTTP fixtures (`tests/dispatch_contract.rs`).
- Requires agent-gateway `f060414` or later: earlier revisions marked a
  cancelled dispatch's task `done`, which amux now refuses as a mismatched
  acknowledgment.
- No background heartbeat, desktop UI or automatic receipt. Recovery is read
  only, and a dead owner's lost generation credentials leave its work fenced, as
  the gateway contract intends.
