# WebSocket sessions and notification delivery

Amux uses `github.com/coder/websocket` for WebSocket transport. Put the shared
server connection lifecycle in `wsock`. Routes authenticate and authorize
the upgrade and own their wire protocol. Amux's router and SQLite outbox hold
durable notifications and delivery state.

## Milestones

1. **Share the connection lifecycle.** Move upgrade handling, frame limits,
   connection tracking, shutdown, and bounded write deadlines into `wsock`.
   Keep the existing task, conversation, navigation, fleet, and remote wire
   protocols. Verify authorization, replay, acknowledgments, and shutdown
   with the existing stream tests.
2. **Bound delivery per connection.** Give each authenticated session one
   writer and a bounded outbound queue where it needs asynchronous delivery.
   Derive session identity from the authenticated principal and fence cleanup
   by connection generation when a reconnect replaces a socket. Keep durable
   notifications pending until acknowledged and replay them after reconnect.
   Coalesce transient wakeups where the protocol permits it. Verify that a
   slow client cannot block unrelated clients and that reconnect preserves
   ordered delivery. The task event stream already offers one durable event
   at a time until acknowledgment.
3. **Route global and agent-specific notifications.** Resolve global audiences
   through authenticated subscriptions. Reuse `agent.notify`, recipient
   bindings, and delivery receipts for notifications addressed to agents.
4. **Harden queue providers as needed.** Bound the memory provider's retry
   backlog as well as its incoming channel. Keep retries ordered, honor
   cancellation and expiry, and avoid calling processors while holding the
   retry lock. Treat SQLite as the durable queue for existing Amux workflows.
   Enable Redis, SQS, or Pub/Sub for a deployment once its backend meets the
   required ordering, retry, and acknowledgment contract.

## Queue ownership

- `wsock.SocketManager` owns live connections and session replacement. It
  publishes a replacement after the authenticated claim succeeds and releases
  a registration only if the closing socket still owns it.
- The remote stream uses one `wsock.Writer` per connection, with eight waiting
  frames and a five-second deadline per write. Saturation closes that stream.
  The durable cursor only advances on acknowledgment, so reconnect can replay
  pending events. Bus events remain wakeups for the durable event scan.
- `queues.Manager.Open` creates a separate queue instance for callers that
  need a provider. The imported global queue and runner-action dispatcher
  were removed when the socket lifecycle moved into `wsock`. The memory,
  SQS, and Pub/Sub implementations are still available. Callers start providers
  explicitly. The factory returns an error for the Redis stub.

## Single-server deployment

Use the memory provider for transient queued work on this server. Keep durable
notifications and replay cursors in SQLite so they survive a restart. SQS and
Pub/Sub remain available for a future deployment that needs a shared queue.
Redis remains a stub.

## Notification API

- An authenticated administrator can post `{"message":"Scheduled maintenance"}`
  to `POST /v1/notifications/global`. A successful response returns HTTP 201
  and the durable event UID. The message can be up to 4096 bytes.
- Authenticated consumers opt in by including `notification.global` in the
  `events` list on `GET /v1/events/stream`. Each consumer has an independent
  acknowledgment cursor. Reuse the consumer ID to resume. A new consumer can
  request `after: 0` to read retained history.
- Enrolled nodes opt in to `agent.notify` and list the relevant session IDs in
  `targets`. The server checks the enrolled instance and the native
  delivery receipt or authenticated presence binding for each requested
  recipient. A shared gateway API key can receive global broadcasts.
- A stream acknowledgment advances the subscriber's replay cursor. Native
  agent delivery and its result receipts continue through the native delivery
  plugin.

## Client delivery

The desktop keeps a notification consumer ID in navigation preferences, scoped
to its gateway and enrolled node when available. It shows each global notice as
a desktop notification and acknowledges it after handing it to the UI.
Reconnecting with the same consumer ID resumes its pending notices.

The memory provider counts all queued and in-flight messages against its
capacity. A failed head waits for a reconnect signal or expiry before later
messages advance. Producers waiting for space can cancel their wait. A
processor owns the lifetime of its callback.

## External channels

The deployment uses the native desktop with the gateway WebSocket stream.
External channel policy is a separate decision under consideration, including
limiting channels such as Discord to status briefs and escalations or removing
them.

## End-to-end checks

- The CLI process test starts `amux server` with a temporary config and
  database. It publishes a global notice, reconnects before acknowledging,
  verifies replay of the same event, then checks the saved cursor after
  shutdown.
- The desktop integration test delivers two notices through the gateway to
  Fyne in order. The second notice can arrive after the first is acknowledged.

## Shared queue

The top-level `queues` package owns the existing provider manager and its
in-memory FIFO. Each caller opens a named topic and injects its processor.
WebSocket writers use `wsock.<connection-id>` for outbound frames and the
router uses `router.<plugin-id>` for delivery dispatch. Both submit messages
through the same admission API. Socket saturation triggers reconnect. The
router keeps delivery claims, retries, and results in SQLite. For Pub/Sub,
the manager appends the topic name to the configured topic and subscription
prefixes. The current deployment uses memory for both consumers.
