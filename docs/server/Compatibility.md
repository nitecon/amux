# Embedded gateway compatibility release

The first embedded release adds `amux server`. It runs the existing gateway
backend inside the amux executable, using the original API-key configuration,
SQLite schema and channel plugins. Desktop and CLI clients continue using their
configured gateway URL and key. Automatic server/client mode and mTLS belong to
the separate second release described in [ServerArchitecture.md](../ServerArchitecture.md).

## Running the compatibility server

Run from a directory containing the gateway's existing `.env`, or supply the
same environment variables to `amux server`. For a new isolated local profile:

```sh
GATEWAY_API_KEY='<your existing configured key>' \
GATEWAY_HOST=127.0.0.1 GATEWAY_PORT=7913 \
DATABASE_PATH=./data/gateway.db amux server
```

`amux server serve` is equivalent. This release preserves the gateway defaults,
including port 7913 and binding to `0.0.0.0` when no host is configured. Existing
remote TLS proxy arrangements remain applicable. It does not yet implement the
secure listeners on 24182/24181 in this compatibility mode. Native secure mode
is separate and described below.

Discord and WhatsApp use their existing environment configuration. Projects,
tasks and storage APIs can start without configured plugins; operations needing
channel delivery retain their original plugin requirements and errors. There is
no replacement internal channel adapter. Browser pages and the gateway's own
updater are omitted; amux retains its native frontend and updater.

Local operator commands retain their arguments under the `server` prefix:

```sh
amux server task-machine issue PROJECT INSTANCE_ID OS
amux server task-machine revoke PROJECT INSTANCE_ID
amux server task claim-info PROJECT TASK_ID
amux server task recover PROJECT TASK_ID --expected-claim TOKEN --reason REASON
amux server user add USERNAME --admin
amux server user list
amux server user reset-password USERNAME
```

User commands accept a password through standard input or the existing
`GATEWAY_USER_PASSWORD` environment variable. The server takes an OS-backed
sidecar lock for its SQLite profile. Stop the previous server before changing
hosts or switching back to the original gateway: the original executable does
not participate in this new lock. Keep a backup and use an isolated copied
profile for cutover testing; never run both channel consumers simultaneously.

## Native secure mode

Settings offers Server, Client and Legacy modes. Fresh installations without
existing gateway configuration start a loopback server. Enable network hosting
and set a hostname or IP reachable by clients before generating a peering key.
Client Settings accepts that address and the key; Generate new replaces the
previous key, and each key expires within ten minutes. Copy does not extend it.
Operational traffic uses mTLS on 24182; public discovery on 24181 carries no
enrollment secret. Existing URL/API-key connections remain in Legacy mode.

To run a headless secure host, supply a persistent profile and installation UUID:

```sh
amux server secure --profile ./server-profile \
  --instance-id '<persistent installation UUID>' \
  --bind 0.0.0.0:24182 --discovery-bind 0.0.0.0:24181 \
  --name server.example.net
amux server peering-key --profile ./server-profile
amux server peering-key --profile ./server-profile --copy
```

The first peering-key command generates/replaces the invitation; `--copy` prints
the existing active invitation for copying without replacing it. These are local
operator commands using the host's reserved mTLS identity. Keep the profile in a
persistent, owner-controlled directory. The native child uses its own
`application.sqlite` and security database rather than an inherited database path.
An existing compatibility database is not automatically moved into this profile.

The owning desktop stops its child on close or mode change. A headless process
remains owned by its operator; another amux process can attach without taking
ownership or stopping it. Keep external plugin settings available to the host.
When WhatsApp ingress is needed, pass a separate `--webhook-bind` and configure
the existing app secret; discovery never accepts provider webhooks.

Leaf keys rotate automatically under a stable installation CA. Previous keys
have a bounded overlap, and renewed connections preserve node identity and event
cursors. Unix files use owner-only permissions; Windows private keys use
current-user DPAPI. CA-root rollover, database snapshot download and offline
replication are separate features; see the implementation boundary in
[ServerArchitecture.md](../ServerArchitecture.md).

## Compatibility evidence

The reference is the clean gateway commit
`986d84544aae086579c72349a1afc366cef6b589`, rather than the dirty sibling working
tree. [GatewayApiContract.json](GatewayApiContract.json) records all 107 backend
method/path operations and the two WhatsApp webhook operations. The backend's
[UPSTREAM.md](../../crates/gateway-backend/UPSTREAM.md) records the extraction boundary.

Run the differential check after building the pinned original gateway and amux:

```sh
python3 scripts/check_gateway_compatibility.py \
  --reference /path/to/pinned/gateway --candidate target/debug/amux \
  --require-positive-coverage
```

The helper freezes both binaries, records their hashes, uses synthetic credentials
and writes its SQLite profiles, logs and report beneath `target/test-fixtures`.
It does not use installed provider settings or a live channel account. Generated
IDs, timestamps and password salts are normalized for separate-profile comparison;
same-profile switch checks compare exact durable rows and schema definitions.

The local differential checks passed with:

- 447 authentication, method and malformed/missing-resource probes.
- Successful HTTP requests covering all 107 backend operations and both webhook
  operations, across 141 core and 17 plugin behavioral cases.
- The unchanged production WhatsApp plugin calling a local Graph test provider:
  seven matching outbound payloads and authorization headers, signed ingress,
  signature rejection, deduplication and durable inbound message storage.
- Four reference → amux → reference → amux stages preserving all 39 tables and
  102 schema entries, existing ownership and events, and rejecting foreign-owner
  writes without changing durable data.

Native client tests additionally exercise REST task/project/requester operations,
WebSocket events, receipts and reconnect, and channel callback/error branches.
The original memory/OKF and offline-machine process smoke tests also pass against
the embedded executable, including restart, extension preservation, conflicts,
tombstones, durable dispatch deduplication and prior-generation recovery.
The upstream task-stream process smoke passes with only browser-page HTML
assertions removed; its API cookie authorization, WebSocket fencing, replay,
requester/delegation, ownership recovery and idle-claim expiry checks remain.

Operation coverage is bounded evidence, not exhaustive behavioral equivalence.
Live Discord/WhatsApp delivery is not established by these local tests. The
upstream unit tests, native client contracts and supported-platform build/release
checks remain required. No mTLS, offline replica, snapshot download or memory
injection completion follows from this compatibility result.
