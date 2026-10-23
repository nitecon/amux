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
amux dispatch act <task> receive
amux dispatch act <task> claim
amux dispatch act <task> start --claim-id <id> --execution-id <uuid>
amux dispatch act <task> succeeded --claim-id <id> --execution-id <uuid> --result "outcome"
amux dispatch act <task> request_cancel --claim-id <id> --result "reason"
amux dispatch act <task> cancel [--claim-id <id> --execution-id <uuid>] --result "reason"
amux dispatch act <task> handoff [--claim-id <id>] --target '<target JSON>' --result "reason"
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

## Requests and recovery

Every `delegate` and `act` request is written to `dispatch/requests/` with a
fresh request ID before it is sent. Its status becomes `accepted` when the
gateway acknowledges exactly what was asked, or `rejected` on a definite 4xx
rejection. A lost reply, a 5xx or a malformed acknowledgment leaves it
`uncertain`: the gateway may have committed it. amux never resubmits on its own.

`replay` resends an uncertain request byte for byte, with the same request ID,
so the gateway returns its original response instead of acting twice. Only the
execution that created a request may replay it. A later launch, even of the same
agent, has a new generation and cannot adopt or resend an earlier generation's
requests or claims; use `recover` to inspect their state instead.

## Acknowledgment checks

A reply is accepted only if all of these hold, otherwise the request stays
uncertain and the command fails:

- the dispatch and task IDs match, and the task is the one acted on;
- `authentication` names this exact session, generation and machine;
- delegation: the exact requested target, destination project and source
  project, with state `pending`;
- `receive`, `claim`, `start`, `succeeded`, `failed`: this generation is the
  destination and the state is the one that operation produces; a claim returns
  a claim ID and no start time; `start` and outcomes echo the claim and
  execution IDs;
- `request_cancel` records a cancellation request; `cancel` reaches `cancelled`;
  `handoff` returns the dispatch to `pending` for the requested new target.

Responses are limited to 4 MiB and the session list to 1,000 entries. Recovery
is refused if its pending or requested work names another generation.

## Limits

- Not run against a deployed gateway or real provisioned credentials; evidence is
  local HTTP fixtures (`tests/dispatch_contract.rs`).
- Since agent-gateway `f060414`, dispatch `cancel` marks the task `cancelled`
  rather than `done`. amux checks the dispatch state, not the task status, so it
  works with either revision.
- No background heartbeat, desktop UI or automatic receipt. Recovery is read
  only, and a dead owner's lost generation credentials leave its work fenced, as
  the gateway contract intends.
