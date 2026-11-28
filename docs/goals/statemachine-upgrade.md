# State machine upgrade milestones

## Terminology and rename scope

| Existing name | Name after the upgrade | Meaning |
| --- | --- | --- |
| Go `router` package and state machine router | `fism` | The finite state machine that owns transitions and reconciliation. |
| State machine plugin | **Controller** | A processor registered with `fism` for one or more event types. |
| State machine plugin registration | **Controller registration** | The subscriptions, dependencies, and injected processor for a controller. |

Rename the Go package to `fism` and update its imports, references, tests,
and documentation. Use **controller** throughout the state machine
design and registration APIs. Review the exact Go identifiers for controller types and registration at
cutover. **Web router** refers to HTTP and WebSocket route handling. Existing
event IDs, WebSocket routes, and SQLite table names retain their names.

An event type can have multiple controllers, and a controller can subscribe
to multiple event types. For example, a `question` event could have one
controller resolve the recipient and another record the outcome. Independent
controllers run concurrently. Controllers declare dependencies when they
need another controller's result. `fism` waits for required outcomes
before advancing.

**Done when** state machine imports use `fism`, registration uses controller
terminology, and tests cover both directions of the subscription model.

## Boundaries

- **SQLite stores complete states and outcomes only.** The accepted desired
  state is durable. SQLite also holds the last complete observed state and
  outcome. A final write checks that the desired revision and previously
  committed state still match before recording the result.
- **Go owns the transition in progress.** `fism` receives signals over
  channels, chooses the steps between complete states, and coordinates their
  execution. Step labels, controller progress, retry timers, and completion
  signals for an in-progress transition live in memory.
- **Controllers do the work.** The existing queue manager dispatches events to
  injected processors. Independent controllers can run concurrently. `fism`
  waits for all required controller outcomes for an event, then decides
  whether to advance, retry in memory, or commit a complete failure outcome.
- **Subscriptions apply to future events only.** An event uses the active
  subscription list when `fism` accepts it. Controller additions create no
  obligations for previously accepted events. Controller history replay and
  backfill are excluded. When `fism` starts a fresh run, it automatically
  includes active controllers whose subscriptions match events from that run.

## Reconciliation model

A Kubernetes Service of type `LoadBalancer` declares a desired result. The
service controller observes the Service and configures the load balancer
through the cloud provider. It can re-read the provider and work toward the
same result on its next pass. See [Kubernetes controllers](https://kubernetes.io/docs/concepts/architecture/controller/)
and the [cloud service controller](https://kubernetes.io/docs/concepts/architecture/cloud-controller/#service-controller).

For Amux, commit the desired state before acknowledging acceptance. Keep it
available even if the process stops before sending a wakeup. `fism` reads
the desired revision and last complete outcome, observes the relevant domain
or external system, and uses channels and queued controller processors to close
the gap. Signals wake the reconciler and bounded retries handle transient
failures. A new pass checks actual state before issuing side effects. It
settles when the desired state is achieved or the workflow reports a complete
terminal outcome.

## Example

Starting with the current complete state and its last outcome, `fism`
coordinates `pending_submit → submission → confirm_submission → execution`
using Go channels and the event queue. Each step can wait for its associated
controllers before signaling the next step. Once execution settles, `fism`
commits the new complete state and its outcome together. SQLite holds the
accepted desired state and the previous complete observed outcome until that
write succeeds.

## 1. Define complete states and legal transitions

In `fism`, identify which workflow states are complete and map each
transition from one complete state to the next. For each transition, define
its durable desired input, how to observe actual state, the operations needed
to close the gap, and the complete outcomes. Keep intermediate phase names
internal to a reconciliation pass.

**Done when** `fism` tests drive a transition from a complete state through
its steps using channel signals, assert the next complete state and outcome,
and verify that intermediate progress leaves the committed state unchanged.

## 2. Wire controller execution and completion

Open named topics through the existing `queues` manager and inject the
controller processors. Snapshot the subscribed controllers when accepting
each event. Dispatch independent processors concurrently and track completion for
the accepted set in Go. Use the event or transition identity to associate
outcomes with the right run and ignore late or duplicate completions.
Preserve explicit dependency ordering where a controller requires it.

**Done when** tests show that a fast controller completing early does not advance
the parent transition, all required successful outcomes allow it to advance,
and failures follow the transition policy in `fism`. Adding a controller during a transition
affects only events accepted after activation.

## 3. Narrow the SQLite interface to complete-state writes

Replace policy-bearing calls in the existing scheduler
[`storage/router_scheduler.go`](../../storage/router_scheduler.go) with the
reads and atomic writes needed for complete desired states and observed
outcomes. Reuse existing authoritative domain state where it already records
the desired input. `fism` supplies the write, the expected prior state,
and the desired revision it processed. A storage error leaves the
previous complete state intact. While the process remains live, retain the
computed outcome in memory and retry the write without rerunning controllers.

Inventory existing event and delivery records and their readers before
cutover. Identify any records that represent intermediate progress under the
new design and how the cutover will treat those existing records. Agree on
any schema or compatibility changes before implementing them.

Current persistence to account for during cutover:

- Startup retention uses a fresh in-memory `fism` pass. Its controller reads
  retention settings and the committed retention checkpoint, then records the
  completed checkpoint with the cleanup. The startup trigger stays in Go.
- Periodic clock wakeups also stay in Go. The active clock controllers check
  current state on each pass. A failed controller leaves the next tick free
  to reconcile again.
- Native heartbeat intake retains the accepted observation until the active
  controllers settle. Startup reads those observations and opens fresh passes
  from them. Heartbeat dispatch and controller progress stay in Go.
  Remote heartbeat and activity intake use the same ordered source, keeping
  the accepted payload in the observation record and preserving the event
  identity used by the presence cursor.
  Remote and native notification feedback use that source as well. Native
  delivery commits the receipt and accepted feedback in one transaction.
  The assignment controller checks the current offer and task revision before
  applying the feedback.
- Configured provider ingress accepts messages into a durable observation
  record. The inbound source reads those records in order independently of
  presence and feedback. The inbound controller uses the provider message
  identity and updates the project cursor atomically.
- Individual notifications commit the accepted notification and WebSocket
  publication together. `fism` revisits ready recipients from accepted
  snapshots and current domain state. Each pass considers later notifications
  even when an earlier recipient is busy. Delivery receipts and deterministic
  conversation command IDs make a fresh pass idempotent.
- Task reconciliation starts from accepted task snapshots and runs the active
  controllers in Go. The gateway reuses the existing WebSocket publication
  and cursor after a restart. SQLite commits completed controller outcomes and
  removes the accepted snapshot from the outbox atomically.
- Presence keeps the complete accepted observation and its WebSocket
  publication until the active controllers settle. Clock and activity updates
  can commit their observed state and causal broadcast together. Startup
  uses the accepted snapshots to run fresh controller passes. SQLite commits
  terminal outcomes and acknowledges each snapshot in one transaction.
- Legacy review reconciliation reads the current completed task and matches
  its completion revision before dispatching controllers in Go. SQLite retires
  obsolete inputs and commits terminal outcomes with source acknowledgment.
  A fresh pass observes the current task and preserves the WebSocket sequence
  for the same completion revision.
- Startup recovers valid accepted inputs with unfinished `router_deliveries`
  rows and retires their intermediate progress in the same transaction. New
  passes use the active registry and current domain state. Startup and clock
  signals are generated anew. Terminal outcomes and WebSocket publications
  survive cutover, and retention protects the recovered inputs until settlement.
- Gateway WebSocket subscribers read `router_events` using their own cursors.
  Keep that journal and its acknowledgment contract. Controller recovery
  starts from complete state and emits fresh events with the active registry.
  Global notifications write directly to the WebSocket journal and do not
  create controller work.

**Done when** storage tests verify atomic snapshot-and-outcome writes, stale
write rejection, and unchanged stored state during an in-progress transition.

## 4. Define recovery and verify the cutover

Load the accepted desired state and last complete observed outcome at
startup. Reconcile outstanding desired revisions by reading current state
from the system the controllers manage. If a prior pass already produced the
expected effect, recognize it and commit the outcome. If work is still
needed, run the necessary transitions again from the last complete state,
using current observations to avoid repeating effects. Use stable resource
identities or idempotency keys when an external operation can succeed before
the process records its outcome.

The next run starts over from the recorded state. It discards the previous
run's progress, queued work, and controller subscription snapshot. A controller installed
after shutdown participates automatically when it subscribes to an event
emitted by the new run.

Before implementation, identify the authoritative desired-state record and
actual-state lookup for each workflow. Channel wakeups carry no unique input
needed for recovery.

**Done when** integration tests cover a crash before dispatch, during controller
execution, and after controller completion but before the final state write.
Verify the accepted desired state and last outcome remain readable, an
already-applied effect is recognized on the next pass, stale results cannot
settle a newer revision, and a restart with a changed registry dispatches
only freshly emitted events to newly added controllers.
Run the relevant race tests, the full Go suite, and the desktop build.

See the [state machine design](../StateMachine.md) and the
[shared queue and WebSocket design](../websockets.md).
