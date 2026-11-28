# Native backend hosting

`amux server` hosts the backend natively inside the amux executable, with its
SQLite schema and optional channel plugins. Desktop and CLI clients connect in
Server or Client mode over native secure (mTLS) hosting, described below and in
[ServerArchitecture.md](../ServerArchitecture.md).

## Current hosting

Use the desktop **Server** mode for local hosting and to enroll other machines
through **Settings → Gateway**. On another desktop choose **Client** and enter
the gateway address and peering key. Keep the secure and discovery listeners
reachable when enrolling over the network.

Run a standalone gateway with `amux server --config PATH`. Set its SQLite
database or secure profile, listeners, and TLS settings in YAML. Use
`amux headless` for an enrolled worker client. See the
[deployment guide](../../deploy/README.md) for current setup and the
[upgrade procedure](../../deploy/README.md#upgrade-an-existing-deployment)
before opening an existing profile with a new binary.

## Compatibility evidence

The reference is the clean gateway commit
`986d84544aae086579c72349a1afc366cef6b589`, rather than the dirty sibling working
tree. [GatewayApiContract.json](GatewayApiContract.json) records all 107 backend
method/path operations and the two WhatsApp webhook operations. The backend's
[UPSTREAM.md](../../crates/gateway-backend/UPSTREAM.md) records the extraction boundary.

The completed cutover used a real-process differential harness with frozen binary
hashes and isolated SQLite profiles. That harness depended on retired raw HTTP
hosting and has now been removed. The results below are historical acceptance
evidence, not instructions for the current release. Native contract and backend
tests validate the supported hosting and API behavior.

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
