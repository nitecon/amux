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
- Plugins never call each other. They communicate only by emitting events.
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
  channel plugins (Discord/WhatsApp) stay as they are.

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
  `completed`, `stopped`, `skipped`) and retry/replay unfinished work.
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
```

- Phases run in order: all `pre_exec`, then `exec`, then `post_exec`. Steps
  within a phase also run strictly in list order; there is no step-level
  parallelism.
- A plugin completes when every step returns `Continue`.
- `Stop` ends the plugin for this event without running later steps, and its
  dependents are `skipped` (e.g. an eligibility check that finds nothing to do,
  or a veto).
- `Fail` aborts this run; the whole plugin run is retried from its first step
  (see delivery), so steps must be idempotent.
- A plugin may subscribe to one or more events. Each subscription is an
  independent subscriber: it receives and consumes its own event stream. The
  router never correlates or waits across different events; a plugin that
  cares about several events handles de-duplication and ordering itself (in
  its own state).
- A plugin's state lives in its own tables; it never reads another plugin's
  tables.

### 4. `depends_on` (replaces ranking)

- `depends_on` is declared per subscription and names other plugins subscribed
  to the **same event**. The dependent plugin runs only after all of its
  dependencies **completed** for that event instance.
- `depends_on` is optional. A subscription without it runs as soon as the event
  arrives, in parallel with everything else, and is never held or skipped by
  another plugin's retry or `Stop`. Only plugins that opt in wait.
- If a dependency returns `Stop`, its dependents are `skipped` for that event.
  This is the veto mechanism: e.g. a threat-model plugin that assignment
  depends on can block an assignment.
- While a dependency is retrying after `Fail`, its dependents wait.
- The graph is validated when plugins are registered at startup: cycles and
  dependencies on unregistered plugins are startup errors.

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
    A known consumer resumes after its last ack; a new one starts after the
    newest stored event unless `after` is given. A newer connection for the
    same consumer takes over. Only forwarded event ids are accepted.
  - `{"type":"ack","seq":n}` acknowledges the event in flight.
  - `{"type":"emit","id","version","payload","correlation"?}` emits a
    `remote_emit` event; the reply is `{"type":"emitted","uid"}` or an error.
  - `{"type":"heartbeat"}`.
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
  Retries continue at the 30 s cap until the run completes or stops; nothing is
  dropped. When a delivery first reaches the cap, the router emits
  `router.retrying` for visibility.
- The same backoff applies to remote consumer deliveries that are not acked.
- Plugins must be idempotent, keyed on `event_uid` (e.g. review creation
  already deduplicates by source task and `done_at`).
- Retention: completed events are pruned by the existing retention worker.

## Initial event catalogue

| Event | Emitted by | Notes |
|---|---|---|
| `clock.tick` | Clock plugin | coarse interval; the only source of time |
| `task.created` / `task.updated` / `task.commented` | task mutations | from existing triggers |
| `task.assigned` / `task.started` | task mutations | new; `assigned` fires whenever the owner is set or changes (claim or assign) |
| `task.user_review` / `task.completed` / `task.cancelled` / `task.unclaimed` | task mutations | `user_review` is new |
| `client.heartbeat` | remote client | replaces presence-only heartbeat; carries agent list |
| `terminal.activity` | remote client | per agent: output seen, draft present, process changed |
| `agent.online` / `agent.idle` / `agent.busy` / `agent.gone` | Presence plugin | derived state |
| `agent.notify` | Notifier plugin | targeted; text to inject into an agent terminal |
| `router.retrying` | router | a plugin delivery reached the 30 s retry cap |

## Initial plugins

| Plugin | Consumes | Emits | Replaces (client code) |
|---|---|---|---|
| Clock | (timer) | `clock.tick` | client 1 s scheduling ticks |
| Presence | `client.heartbeat`, `terminal.activity`, `clock.tick` | `agent.online/idle/busy/gone` | `backlog_ready`, idle detection as a scheduling input |
| Assignment | `agent.idle`, `task.created`, `task.unclaimed` | `task.assigned` | `desktop_backlog.rs`, `backlog.rs` FIFO/eligibility |
| Notifier | `task.assigned`, reminder/review events | `agent.notify` | audience selection in `delegation.rs` / `delegation_queue.rs` |
| Reminder | `clock.tick`, `task.started` | `agent.notify` | in-progress reminders, `desktop_recovery.rs` reminders |
| Review | `task.completed` | `task.created` (review task) | `desktop_autoreview.rs`, client review journal |
| Reclaim | `agent.gone`, `clock.tick` | `task.unclaimed` | server 15-minute sweep, now aware of real busy state |
| UI (remote) | `agent.*`, `task.*` | — | desktop polling for activity/task refresh |
| Future: threat modelling | e.g. `task.completed` | its own events | — |

Settings such as automatic assignment or automatic review become per-plugin
server settings (enable/disable), not client preferences.

## Client after the change

The client keeps only what the server cannot observe or do:

- Local process/PTY observation, reported as `terminal.activity` and
  `client.heartbeat` events (`delegation_process.rs`, `readiness.rs`, input tee
  draft detection).
- Delivery of `agent.notify` into the terminal with the existing safety: process
  re-verify, draft guard, bracketed paste (`delivery.rs`, `recipient.rs`,
  `submission.rs`, verified delivery in `desktop_delegation.rs`).
- Manual task UI and CLI (`desktop_tasks.rs`, `task_routing.rs`,
  `desktop_routing.rs`).

Removed from the client (about 3,000–3,500 lines including tests):
`desktop_backlog.rs`, `backlog.rs`, `desktop_autoreview.rs`, the review/backlog
journal in `delegation_queue.rs` / `delegation.rs`,
`gateway.rs::enqueue_completion_review`, the decision half of
`desktop_recovery.rs`, the reminder/recovery block in `desktop.rs`, the legacy
`amux reviews` CLI (`autoreview.rs`, keeping `held_finding`), and their tests
(`tests/backlog_contract.rs`, `tests/autoreview_contract.rs`, parts of
`tests/gateway_contract.rs` and `tests/delegation_queue_contract.rs`).

The client detects support by connecting to `/v1/events/stream`; a legacy
gateway answers 404, and the client then does nothing automatic. This keeps
`/v1/tasks/capabilities` identical to the legacy gateway.

## Delivery plan

Each phase is a full verified release; no phase changes the router once phase 1
ships, other than bug fixes.

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
