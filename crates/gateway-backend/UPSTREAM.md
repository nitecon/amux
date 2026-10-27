# Gateway backend source

Ported from [nitecon/agent-gateway](https://github.com/nitecon/agent-gateway),
commit `986d84544aae086579c72349a1afc366cef6b589`, under Apache-2.0. The accompanying
LICENSE preserves the upstream license. The clean Git object is the reference;
the sibling working checkout was dirty and was not imported.

## Port boundary

Database schema, migrations, indexes/triggers and backend handlers retain their
original behavior. The API router and bearer/session identity boundary come from
the original main module. Browser pages and the gateway executable/updater lifecycle
are excluded. External channel connectors and plugin ingress/worker behavior are
retained for the initial compatibility/cutover phase by explicit owner direction.
Backend password/session helpers remain because API authorization uses them.
Native channel decoupling is deferred until after verified cutover.

Original database and handler tests are retained where they cover the backend.
The memory rollback SQL fixture is relocated under this crate's test fixtures.
The OKF tombstone retry test uses an explicitly historical tombstone timestamp
to exercise the upstream no-op branch independently of wall-clock tick timing;
it also verifies the record and server revision remain unchanged.
No original production database
is imported or modified by these tests.

The audited method/path/handler inventory is in
`docs/server/GatewayApiContract.json` at the amux repository root. Importing source
alone does not establish parity: the real-server/client/differential acceptance
gates must pass before releasing an embedded gateway.
