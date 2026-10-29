# Server State Machine

Status: proposed. This document is the end-to-end requirement and strategy for
the server event router ("state machine") and its plugins. It supersedes the
task scheduling parts of `architecture.md` and `ServerArchitecture.md` until
those are uplifted.

Baseline: file references and sizes are against main at v0.0.42 (705286c).

## Goal

Move every automatic task behaviour (queue pickup, assignment, reminders,
completion review, reclaim, agent activity) out of the desktop client and into
the amux server, built as:

1. A **lean, generic event router** that never changes except for bug fixes and
   optimizations.
2. **Events** as first-class citizens with one contract for defining and
   emitting them.
3. **Plugins**: self-contained consumers that run `pre_exec`, `exec` and
   `post_exec` steps when an event they subscribe to arrives.

New features (threat modelling, etc.) are added as new events and plugins only;
the router and existing plugins are not modified.

## Principles

- The router knows nothing about tasks, agents, idle/busy, reviews or timers.
  It only routes events to subscribers.
- Plugins never invoke another controller's reconciliation steps. Events drive
  changes; explicit read contracts expose authoritative observations such as
  Presence's live-agent snapshot.
- An event with no subscribers is skipped and not stored.
- Delivery is at least once. Every plugin must be idempotent.
- Independent plugins run in parallel; ordering is expressed only through
  `depends_on`.
- KISS: no feature logic in the core, no speculative extension points beyond
  the contracts below.

## Scope

- **In scope:** the embedded/native amux server (`crates/gateway-backend`),
  desktops and CLIs connected to it in Server or Client mode.
- **Legacy external gateway:** gets none of this. Clients connected to a legacy
  gateway do no automatic scheduling or review at all; the user asks an agent
  to do the work manually.
- **Out of scope:** changing the existing task REST API contract; the external
  channel plugins' wire protocols (Discord/WhatsApp) stay as they are. Their
  internal message ingestion uses the same durable controller machinery.
  TLS credential repair, parent leases and socket keepalives belong to
  transport hosting, independent of domain controllers.

## Core router

The router is the only component that is "the state machine".

```text
emit(event) -> validate -> lookup subscribers(event.id, version)
            -> none: skip (nothing stored)
            -> persist event + one pending delivery per subscriber
            -> build dependency graph from depends_on
            -> run each plugin when its dependencies have completed
               (independent plugins in parallel)
            -> record per-plugin outcome; event complete when all settled
```

Responsibilities, and nothing more:

- Accept events from the emitter (server code and remote clients).
- Persist each event that has subscribers, with its deliveries, before
  acknowledging the emit.
- Resolve subscribers from the registry.
- Schedule plugin execution per the `depends_on` graph.
- Record per-plugin delivery state (`pending`, `running`, `retrying`,
  `completed`, `stopped`, `skipped`, `failed`) and retry/replay unfinished work.
- Forward events to remote consumers that subscribed to them.

It holds no timers (time comes from a Clock plugin) and no domain state.

## Contracts

### 1. Event definition

```rust
struct Event {
    id: EventId,          // event type, e.g. "task.completed"; dotted, lowercase
    version: u32,         // payload schema version
    event_uid: Uuid,      // unique per emitted instance; idempotency key
    payload: Value,       // validated against the registered schema
    source: Source,       // server plugin id, or authenticated remote client
    emitted_at: i64,      // ms since epoch
    correlation: Option<Uuid>, // uid of the event that caused this one
}
```

- Every event id is declared once in an event registry with its version and
  payload schema. Emitting an undeclared id or an invalid payload is rejected.
- Event ids are append-only: a breaking payload change is a new version, and
  consumers state the versions they accept.
- `correlation` lets a chain of events (e.g. `task.completed` ->
  `task.created` review) be traced end to end.

### 2. Emitter

- Server code: `bus.emit(event)` returns once the event is persisted.
- Plugins emit through the step context (`ctx.emit(event)`), which sets
  `source` and `correlation` automatically.
- Remote clients emit with an `emit` frame on `/v1/events/stream` (below). The
  server sets `source` from the authenticated principal (`client:<node>` for
  mTLS peers, `client:api-key` for the bearer key); a client cannot forge
  another source. Only event ids marked `remote_emit: true` in the registry are
  accepted from clients.
- Existing task REST mutations keep their API. SQLite triggers write each task
  change to `router_task_outbox` in the mutation's own transaction, and the task
  event source drains it into `task.*` events, so an event is never lost or
  emitted for a rolled-back change. The legacy-compatible `task_events` journal
  and `/v1/tasks/stream` are not changed.

### 3. Plugin (event consumer)

```rust
trait Plugin {
    fn id(&self) -> &'static str;
    fn subscriptions(&self) -> Vec<Subscription>;
    fn retry_policy(&self) -> RetryPolicy; // defaults to Forever
    fn fifo(&self) -> bool; // defaults to false
    fn pre_exec(&self) -> Vec<Box<dyn Step>>;
    fn exec(&self) -> Vec<Box<dyn Step>>;
    fn post_exec(&self) -> Vec<Box<dyn Step>>;
}

struct Subscription {
    event: EventId,
    versions: RangeInclusive<u32>,
    depends_on: Vec<&'static str>, // plugin ids that must complete first for this event
}

trait Step {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome;
}

enum StepOutcome { Continue, Stop, Fail(String) }
enum RetryPolicy { Forever, MaxAttempts(NonZeroU32) }
```

- Phases run in order: all `pre_exec`, then `exec`, then `post_exec`. Steps
  within a phase also run strictly in list order; there is no step-level
  parallelism.
- A plugin completes when every step returns `Continue`.
- `Stop` ends the plugin for this event without running later steps, and its
  dependents are `skipped` (e.g. an eligibility check that finds nothing to do,
  or a veto).
- `Fail` aborts this run. The plugin's retry policy either retries from its
  first step or records a terminal `failed` delivery with its last error.
  Steps must be idempotent because retries and restart recovery replay runs.
  `MaxAttempts` counts failed runs including the initial run; one lets it fail
  without retrying. `Forever` preserves the default controller behaviour.
- A plugin may subscribe to one or more events. Each subscription is an
  independent subscriber: it receives and consumes its own event stream. The
  router does not correlate different event types. A plugin can opt into
  global FIFO delivery: its oldest unfinished event blocks its later events,
  including during retries. Other plugins remain parallel. Indexed head
  selection keeps a blocked FIFO backlog out of runnable claim pages.
- A plugin's state lives in its own tables; it never reads another plugin's
  tables directly. Presence exposes a typed live-agent snapshot read contract
  (`presence::agents`) for controllers, rather than separate copies of the
  same busy/idle state. Assignment, Reminder, Reclaim and Notifier use it.

### 4. `depends_on` (replaces ranking)

- `depends_on` is declared per subscription and names other plugins subscribed
  to the **same event**. The dependent plugin runs only after all of its
  dependencies **completed** for that event instance.
- `depends_on` is optional. A subscription without it runs as soon as the event
  arrives, in parallel with everything else, and is never held or skipped by
  another plugin's retry or `Stop`. Only plugins that opt in wait.
- If a dependency returns `Stop` or exhausts its retry policy, its dependents
  are `skipped` for that event.
  This is the veto mechanism: e.g. a threat-model plugin that assignment
  depends on can block an assignment.
- While a dependency is retrying after `Fail`, its dependents wait.
- The graph is validated when plugins are registered at startup: cycles,
  subscriptions matching no declared version, and dependencies missing any
  consumed event version are startup errors. Recovery skips unfinished
  deliveries whose plugin no longer subscribes to their event version.

### 5. Remote consumers

- Clients (desktop UI, terminal delivery, CLI watchers) connect to
  `/v1/events/stream`, a WebSocket authenticated like `/v1/tasks/stream`
  (mTLS peer or bearer key). It is a separate endpoint so the legacy-compatible
  stream contract stays unchanged.
- Remote delivery is itself a plugin (`remote`): it subscribes to every
  forwarded event id, so those events are stored, and wakes connections. Each
  consumer keeps a durable cursor in `remote_consumers`.
- Frames, client to server:
  - `{"type":"subscribe","consumer_id":uuid,"events":[ids],"targets":[session ids],"after":seq?}`.
    A consumer seen within the last 24 hours resumes after its last ack; a new
    or expired one starts after the newest stored event unless `after` is given. A newer connection for the
    same consumer takes over. Only forwarded event ids are accepted.
  - `{"type":"ack","seq":n}` acknowledges the event in flight.
  - `{"type":"emit","id","version","payload","correlation"?}` emits a
    `remote_emit` event; the reply is `{"type":"emitted","uid"}` or an error.
  - `{"type":"heartbeat"}`.
- A successful `subscribed` frame includes `server_restart_reminders`, based
  on whether Reminder subscribes to heartbeat v1. Absence means an older router
  and selects the client compatibility journal; it does not enable assignment.
- Server to client: `subscribed`, `event` (`seq`, `id`, `version`, `uid`,
  `payload`, `source`, `emitted_at`, `correlation`), `acked`, `emitted`,
  `heartbeat`, `error`.
- One event is in flight per consumer. Until it is acked it is resent with the
  router backoff (1 s doubling to 30 s), and it is resent after a reconnect.
- Events whose payload has a `target` (agent session id) go only to consumers
  that listed that target; other events go to every subscribed consumer.
- Acks are transport-only. A client reports an outcome (e.g. a notification
  skipped because the agent turned busy) by emitting an event correlated with
  the one it answers; the originating plugin consumes that.
- The UI is just a consumer: e.g. it updates an agent's activity indicator on
  `agent.busy` / `agent.idle`.

## Delivery guarantees

- **At least once.** Events are persisted before the emit returns. Per-plugin
  and per-remote-consumer delivery state is persisted.
- On restart the router replays every event that has an unfinished plugin or
  consumer delivery.
- Failed plugin runs are retried with exponential backoff: first retry after
  1 s, doubling each time (1, 2, 4, 8, 16 s), capped at 30 s between attempts.
  By default retries continue at the 30 s cap until the run completes or stops.
  A plugin may instead declare `MaxAttempts`; exhausted deliveries retain
  their failure and settle their dependents. When a retrying delivery first reaches the cap, the router emits
  `router.retrying` for visibility.
- The same backoff applies to remote consumer deliveries that are not acked.
- Plugins must be idempotent, keyed on `event_uid` (e.g. review creation
  already deduplicates by source task and `done_at`).
- Retention: settled events are pruned by the Clock-driven Retention plugin
  only after every unexpired remote consumer has advanced past them. Client
  traffic renews a consumer's 24-hour replay lease. Recent offline consumers
  protect their unread range; Retention deletes expired registrations in the
  same transaction as cleanup, so abandoned desktops cannot retain history
  forever. An expired consumer resumes at the tail by default; explicit `after`
  can request whatever history remains. Unfinished plugin deliveries survive
  consumer expiry.
- The router scans pending work in bounded pages, runs at most 64 deliveries
  concurrently and owns their tasks. Delayed work does not hide other
  plugins’ runnable deliveries; an opted-in FIFO plugin holds its own tail.
  Shutdown cancels controller execution; interrupted deliveries replay after
  restart. Recording an outcome retries independently after storage failures,
  so a failed bookkeeping write does not rerun completed plugin steps.

## Event catalogue

All events are version 1. Payload schemas are validated by the registry.

| Event | Emitted by | Payload |
|---|---|---|
| `clock.tick` | Clock source, every 15 s | `{at}`; the only source of time |
| `retention.startup` | server runtime | `{}`; requests immediate startup cleanup |
| `channel.message_received` | channel input adapter | `{channel_name, room_id, message: {id, content, author_kind}}`; internal only |
| `task.created` / `task.updated` / `task.commented` / `task.assigned` / `task.started` / `task.user_review` / `task.completed` / `task.cancelled` / `task.unclaimed` | task event source (outbox) | `{project, task, comment, actor}`; `assigned` fires whenever the owner is set or changes (claim or assign) |
| `client.heartbeat` | remote client (`remote_emit`) | `{client_run?: uuid, agents: [{session_id, instance_id, provider, os, project, name?, goal?, agent_id?, generation?, canonical_remote?}]}`, every agent the client runs; `client_run` identifies one desktop lifetime across reconnects |
| `terminal.activity` | remote client (`remote_emit`) | `{session_id, instance_id, output_at, draft}`; `output_at` is the last terminal output on the server's clock |
| `agent.online` / `agent.idle` / `agent.busy` / `agent.gone` | Presence | `{agent}` as reported in the heartbeat, plus its `source` |
| `agent.notify` | Assignment, Notifier, Reminder | `{target, project, mode, event, heading, instructions, require_idle}`; `event` is the task-event shape terminals already render, `mode` is `assigned` (already claimed for the agent) or `recipient` |
| `agent.notify_result` | remote client (`remote_emit`) | `{target, status, reason}`, correlated with the `agent.notify` it answers; `status` is `delivered`, `skipped` or `failed` |
| `router.retrying` | router | `{event_uid, event, plugin, attempts, error}` when a delivery reaches the 30 s retry cap |

## Plugins

| Plugin | Consumes | Emits | Replaces (client code) |
|---|---|---|---|
| Clock (source) | (timer) | `clock.tick` | client 1 s scheduling ticks |
| Task events (source) | outbox rows | `task.*` | — |
| Channel input (source) | bounded transport queue | `channel.message_received` | direct message/cursor database writes in runtime |
| `inbound` | `channel.message_received` | — | direct runtime message ingestion |
| `remote` | every forwarded event | — (wakes stream connections) | — |
| `presence` | `client.heartbeat`, `terminal.activity`, `clock.tick` | `agent.online/idle/busy/gone` | `backlog_ready`, idle detection as a scheduling input |
| `assignment` | `clock.tick` (depends on `presence`), `agent.notify_result` | `agent.notify` | `desktop_backlog.rs`, `backlog.rs` FIFO/eligibility |
| `notifier` | `task.created/commented/completed` | `agent.notify` | audience selection in `delegation.rs` / `delegation_queue.rs` |
| `reminder` | `clock.tick`, `client.heartbeat` (depends on `presence`) | `agent.notify` | periodic updates and current-server restart reminders |
| `review` | `task.completed` | (review task via `task.created`) | `desktop_autoreview.rs`, client review journal |
| `reclaim` | `clock.tick` (depends on `presence`) | (`task.unclaimed` via the task store) | the 15-minute sweep and per-request reclaims |
| `retention` | `retention.startup`, `clock.tick` | — | independent daily server cleanup timer |
| UI (remote) | `agent.*` | — | local activity indicator |
| Future: threat modelling | e.g. `task.completed` | its own events | — |

Rules carried over from the desktop:

- **Presence:** an agent is idle when its terminal has had no output for 30 s
  and it has no draft; until its first activity report its state is unknown.
  It is gone 90 s after its client's last heartbeat. Only the client that
  reported an agent may report its activity, and output time only moves
  forward. Durable source-heartbeat and session-activity sequence checks
  reject reordered or replayed reports. Evaluation time never moves backward;
  a delayed heartbeat may restore a session only while that report is fresh.
- **Assignment:** every 15-second tick runs Presence first, then sweeps all
  reported sessions across every workspace/project. Each currently idle agent
  that owns no in-progress task gets the oldest eligible queued task in its
  project (OS label, review exclusion, goal, delegation, owner and session
  routing). Queued `autoreview-finding` tasks remain ordinary work; **User review**
  is the explicit parked state. It claims the
  task as that agent through the normal task store, comments why, and sends
  `agent.notify` with `require_idle`. A `skipped` or `failed` result releases
  the claim; the next tick revalidates idle sessions and offers queued work.
  The refusing session is skipped until its terminal reports new output, so
  other idle sessions receive the task instead of the same refusal repeating.
  Sessions are considered oldest terminal output first, then session ID. The
  sweep fills every eligible free session while work remains; empty or blocked
  projects do not stop other workspaces. Fresh Presence state is authoritative,
  including sessions that stayed idle since the previous tick. Activity and task
  events do not independently trigger assignment. The first clock tick is 15 s
  after startup; missed ticks are skipped rather than replayed in a burst.
  Claim conflicts skip an ineligible task; storage failures fail the delivery
  for retry. A rejected notification consumes its offer and releases the task
  in one transaction, preserving compensation context if either write fails.
  Results must match the offer's captured authenticated source and target as
  well as its notification UID, even after Presence disappears. Legacy offers
  without captured reporter authority retain their claims but cannot authorize
  a callback transition; ordinary task actions or expiry retire them.
  Any subsequent task or comment mutation invalidates the old offer in that
  mutation's transaction, so a delayed rejection cannot undo progress or a
  newer claim, even when timestamps match.
- **Notifier:** owners hear about activity on their tasks, explicit requesters
  receive comment/completion results, actors never hear about their own
  changes, and queued or user-review work is left to Assignment and the user.
  Recipients come from live Presence at execution time, including replayed
  task events; there is no separately maintained identity projection. A source
  event's completion marker commits with all notifications, so replay after a
  crash cannot emit a second set of notification UIDs. Retiring the source
  event also removes that marker.
- **Reminder:** in-progress work with no task update, comment or reminder for
  ten minutes prompts its connected exact owner. Delivery requires idle.
  A new `client_run` also prompts a resumed
  session about its in-progress work, once per execution and desktop lifetime.
  A fresh instance gets an explicit prior-execution notice addressed to that
  receiver; this does not transfer task ownership. Restart notices are
  informational and wait for idle. Reconnects do not create a new run, stale
  heartbeats cannot reintroduce one, and emission plus deduplication commit
  together. Clients sharing a reporting source retain independent run markers.
  Older clients without `client_run` keep periodic reminders.
- **Reclaim:** claimed work returns to the queue after 15 minutes without task
  activity, except while its owner is busy. With the router running, the
  Clock-driven controller owns expiry. REST reads and task mutations never
  perform an implicit reclaim, including API-only component instances.
- **Retention:** cleans messages under the existing retention policy at
  startup and daily through Clock. Cleanup and its last-success marker commit
  together; failures retry through the router. Unfinished events are retained.
- **Inbound:** input persistence retries with bounded queue backpressure.
  Once accepted by the bus, insertion and backfill cursor advancement commit
  together and retry durably. Inbound opts into global FIFO, preserving the
  previous serial ingestion order and unread message IDs. A retry holds later
  input while other controllers continue. Deliveries cannot rewind the cursor;
  provider duplicates do not insert or advance it. The adapter starts before
  channel backfill to avoid filling the startup queue before it can drain.

### Agent identity and leases

The server has one identity directory in `task_target_presence`. The legacy
REST target adapter and event-driven Presence use explicit read/write contracts
for this shared domain resource; controllers do not maintain identity copies.

- A routing advertisement retains its validated repository binding, exact
  origin/generation and 120-second lease. Manual routing uses this contract.
- An event report has its own authenticated source, activity state and
  90-second lease on the same identity row. Automatic controllers use this
  observation contract; a REST-only advertisement supplies no idle evidence.
- Existing v1 event-only clients remain supported. Observation-only rows do
  not grant validated REST routing authority. Native reports include their
  confirmed repository binding when available.
- Advertisement and event updates only write their own lane; delayed REST
  advertisements cannot erase newer busy/source observations. Registered
  hostname history survives lease expiry and event departure as display
  provenance, independently of routing authority. Event-only observations
  cannot forge a registered hostname.
- Machine summaries count fresh heartbeat sources independently of agent rows,
  including empty inventories. Optional heartbeat `os` describes a host before
  any agent is discovered. Native observations with unresolved projects still
  count as agents, but task controllers wait for project binding. Unresolved
  observations cannot claim work, suppress reclaim, or consume restart-reminder
  deduplication.
- Credential-bound `task_sessions` represents enrollment and revocation,
  rather than a second presence directory, and stays separately scoped.

Repository normalization lives in `projects::canonical_remote` and is reused
by the client: default protocol ports normalize, custom endpoints remain
distinct, and URLs with query/fragment/password metadata are rejected. Ordinary
task OS requirements live in the backend `task_platform` domain module. Client
JSON/CLI adapters and server controllers use that validation rather than
maintaining separate eligibility rules. Local read-only Git process handling
is shared by checkout and gateway identity observation.

Plugins whose behaviour a user may want off (`assignment`, `notifier`,
`reminder`, `review`, `reclaim`) use an `Enabled` pre-exec step for the server
setting `plugin.<id>.enabled`. They are enabled by default. For Reminder this
switch gates periodic nudges only; startup continuity remains available, as
in the original desktop restart behavior.

## Client after the change

The client keeps only what the server cannot observe or do:

- Local process/PTY observation: the agent scan binds each terminal to its
  native process and identity (`delegation_process.rs`), the input tee tracks
  typed drafts, and both are reported as `client.heartbeat` and
  `terminal.activity` (`desktop_events.rs`, via `event_client.rs`).
- Delivery of `agent.notify` into the named agent's terminal with the existing
  safety: native process re-verify, draft clear/restore in one ordered write,
  bracketed paste, never Ctrl+C. The client acknowledges each `agent.notify` as
  soon as it is queued locally, so one agent's wait never holds up the stream;
  pending notices end with the app and are not replayed. A resubscription to
  the same logical connection preserves queued notices and delivered-UID
  suppression; a connection/identity reset clears them.
  `require_idle` notices are only delivered while the agent is idle: an
  `assigned` work offer to a busy agent is reported `skipped` so the server can
  offer it elsewhere, and an informational notice (a requester result) waits
  for the agent to go quiet within its five-minute lifetime.
  Eligibility is checked again after asynchronous process verification. Each
  verification attempt carries its own token, and in-flight notifications
  participate in duplicate suppression. The intended session, instance and
  project are frozen on receipt and checked again before paste; requester
  notices also retain their execution-generation fence. A changed native
  attachment can resume only for the same logical owner execution.
- The UI as a consumer: server `agent.idle` / `agent.busy` drive each terminal's
  activity indicator, and `task.*` events refresh that project's task panel.
- Manual task UI and CLI (`desktop_tasks.rs`, `task_routing.rs`,
  `desktop_routing.rs`).
- One process-lifetime `client_run` value reported in heartbeats. A successful
  subscription advertises `server_restart_reminders`; supporting servers own
  restart eligibility and task lookup. When an older event router omits this
  flag, the retained local restart journal provides informational continuity
  after idle. Negotiation selects one authority; a 404 enables no fallback.

Removed from the client in phase 5: the desktop backlog scheduler, client
autoreview and its journal, the legacy task-stream delivery journal
(`delegation_queue.rs`) and its desktop bridge, the opt-in reminder/dispatch
recovery path (`desktop_recovery.rs`) with its delivery ledger,
`Gateway::enqueue_completion_review`, the Autoreview setting, and the legacy
`amux reviews` CLI (`autoreview.rs` keeps `held_finding` and the guidelines
text). `delegation_connection.rs` remains as the client for the
legacy-compatible `/v1/tasks/stream`, which the server still serves.

The `delivery`, `readiness`, `submission`, `recipient` and
`delegation_readiness` library primitives and their contract probes remain
available for the planned memory/context work. Removing the old desktop
integration did not retire these APIs. Current automatic task notifications
use the event delivery boundary above, rather than that provisional prompt
classifier or local ledger. Raw legacy stream filters remain compatibility
helpers, with no automatic scheduling authority.

The client detects support by connecting to `/v1/events/stream`; a legacy
gateway answers 404, and the client then does nothing automatic. This keeps
`/v1/tasks/capabilities` identical to the legacy gateway.

## Architecture checks

The application and backend test suites exercise these boundaries directly:

| Invariant | Evidence in the repository |
|---|---|
| Durable outcomes, dependency ordering, finite/indefinite retries, bounded claims and owned worker shutdown | `router.rs` unit tests |
| One identity directory, independent leases, compatible v1 reports, stale-report fencing and schema upgrades | Presence unit tests and task-routing contracts |
| Controllers own expiry, reminders, retention and ingestion; task reads do not reconcile ownership | Reclaim/Reminder/Retention/Inbound tests and the task-read purity regression |
| Notification batches and assignment compensation are atomic and replay-safe | Notifier and Assignment fault-injection tests |
| Clients observe/report and deliver safely across reconnects and process changes | Native desktop delivery tests and `tests/event_stream_contract.rs` |
| Repository identities and OS requirements agree across client/store/controllers | Project and platform tests; Git/project identity contracts |
| Database upgrades retain user data and reject overwriting active work | `tests/migration_contract.rs` |

Run `cargo test -p amux -p amux-gateway-backend`,
`cargo clippy -p amux -p amux-gateway-backend --all-targets -- -D warnings`,
and `cargo fmt --all -- --check` for the application/backend gates.

## Historical behavior preservation

The consistency refactor was checked against the changes that introduced the
behavior, not only the current call graph:

| Prior contract | Preserved behavior and checks |
|---|---|
| `4839646`, `b9de9d1`: restart continuity | Stable-session reminders survive instance replacement and same-client resubscription; old-router fallback remains, without ownership transfer. |
| `b6428fc`: periodic owner nudges | Exact live owners receive periodic updates; disabling periodic reminders does not suppress startup continuity. |
| `32a29df`: automatic hostname provenance | Trusted registration metadata survives routing lease expiry; event-only reports cannot invent it. |
| `921ea75`, `21662f6`: native delivery and planned primitive reuse | Immediate native notifications remain; public safety primitives and installed-provider probes are retained for context work. |
| `eae8ff1`: server-owned task scheduling | The retired desktop assignment/review/recovery UI stays retired; its removal does not retire unrelated library APIs. |
| Serial channel ingestion | Opt-in FIFO preserves message ID/unread ordering across retries, while other controllers run independently. |

One upgrade limitation is deliberate: pre-upgrade assignment offers lacking
captured reporter authority cannot authorize rejection callbacks. Source history
alone cannot prove that the claim has had no later mutation. Their claims and
rows are retained for ordinary task actions or expiry rather than inferring
authority. The callable storage reclaim utility remains available; request-time
implicit reclaim is replaced by the running server’s controller.

## Delivery plan

Each phase is a full verified release; no phase changes the router once phase 1
ships, other than bug fixes.

Status: phase 1 shipped in v0.0.43, phase 2 in v0.0.44. Phases 3 and 4 were
tagged as v0.0.45 and v0.0.46, but their release builds stopped on a Presence
test that assumed the test's clock instead of the heartbeat's server time on
slower runners; phase 5 was tagged v0.0.47 with that fix but its build stopped on
macOS/Windows-only lint in the desktop; v0.0.48 ships phases 3–5.

| Phase | Deliverable | Acceptance |
|---|---|---|
| 1. Router | Event registry, emitter, plugin/step contracts, `depends_on` graph and validation, parallel runner, persistence, replay, retry, Clock plugin, test plugins | Unit and restart tests: no-subscriber skip, parallel independents, dependency wait, stopped dependency skips dependents, retrying dependency holds dependents, backoff 1 s doubling to 30 s, cycle rejected at startup, at-least-once after kill/restart, idempotent replay. No behaviour change for users. |
| 2. Event sources and remote consumers | Task mutations emit `task.*` via outbox; `remote` plugin and `/v1/events/stream` with subscriptions by event id and target, ack/resend and cursor resume; remote emit of `client.heartbeat` / `terminal.activity` | Real two-client test: every task mutation emits exactly one event; each consumer receives only what it subscribed to; unacked events are resent; reconnect resumes after the last ack; clients can emit only `remote_emit` events with valid payloads. |
| 3. Client connection, Presence, Assignment, Notifier | Desktop connects to `/v1/events/stream` on embedded/native servers, emits `client.heartbeat` / `terminal.activity`, delivers `agent.notify` and reports the outcome; UI activity consumer; plugins ported from client logic (FIFO, OS, review exclusion, one task per idle agent) | Real-server test: idle agent receives the oldest eligible task; busy or drafting agent does not; a skipped delivery leads to re-offer; UI activity follows `agent.*`. |
| 4. Review, Reminder, Reclaim | Plugins; 15-minute sweep moves into Reclaim | Completed task creates exactly one review; a busy owner's task is not reclaimed; a gone owner's task is. |
| 5. Client strip | Remove client scheduling/review code and settings; journal format simplified | Desktop against embedded server behaves as phases 3–4; against a legacy gateway does nothing automatic; full core/desktop suites pass on Linux, Windows, macOS. |

Phase 5 touches `desktop.rs` heavily. The legacy gateway migration wizard it
previously had to wait for landed in v0.0.42 (705286c) and touched none of the
scheduler files listed above.

## Size estimate

| Piece | Code (lines) | Notes |
|---|---|---|
| Router and contracts | 500–800 | the critical part; heaviest tests |
| Plugins | 1,000–1,300 | presence ~200, assignment ~400, notifier ~150, reminder ~150, review ~100, reclaim ~100, clock ~50 |
| Stream/remote consumer extensions | ~250 | |
| Client removal | −3,000 to −3,500 | plus ~1,200 lines split |

Tests are expected to equal or exceed the code size, following the existing
backend modules.

## Open questions

- Whether `router.retrying` should surface in the UI (it is available to any
  remote consumer either way).
- Channel providers retain their existing acceptance behavior: their enqueue
  acknowledgment precedes durable bus persistence. The input adapter retains
  and retries accepted input while running; a crash before persistence still
  relies on provider redelivery/backfill. Strengthening provider acknowledgments
  would change the transport contract and is outside this refactor.
