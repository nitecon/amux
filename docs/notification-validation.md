# Desktop notifications

`src/notifications.rs` delivers local desktop notifications. It follows the
[notification module boundary](architecture.md#notification-module-boundary):
amux shows desktop notifications only; external notifiers such as Discord stay
with the gateway. It works without a gateway and without provider hooks. The
desktop UI does not call it yet; wiring it in is separate work.

## Interface

```rust
let mut notifier = Notifier::native();          // or Notifier::new(backend)
let id = notifier.notify("Title", "Body")?;     // never blocks
for outcome in notifier.outcomes() { /* Shown, Failed or TimedOut, each with its id */ }
```

- `notify` queues one attempt and returns its ID immediately. It returns
  `Dropped::QueueFull` when 16 attempts are already waiting, `Dropped::EmptyTitle`
  for a blank title, and `Dropped::WorkerStopped` if the worker has exited.
- A worker thread owns the queue and a single attempt thread calls the OS, in
  order. A missing, slow, failing or panicking notification service cannot block
  the caller, terminals or task operations. If the worker cannot start, every
  `notify` returns `WorkerStopped`; setup never fails the app.
- Each OS call has a 5-second deadline. Past it, the attempt reports `TimedOut`:
  the notification may or may not appear. While that call is still outstanding,
  later attempts report `Failed` at once instead of starting more threads; at
  most two threads exist per notifier.
- Each attempt runs once. Failures are reported, never retried.
- `Shown` means the OS notification service accepted the request. It does not
  mean the user saw it, and no outcome ever means a task was received, claimed
  or started.
- Titles are limited to 120 characters and bodies to 1,000, with a `…` where text
  was cut. Control characters are removed; bodies keep line breaks.

## Platforms

| Platform | Backend | Known limits |
| --- | --- | --- |
| Linux | freedesktop notification service over the D-Bus session bus (`notify-rust`, pure Rust) | Needs a running notification daemon; without one, attempts report `Failed`. `&`, `<` and `>` in the body are escaped so they show literally |
| Windows | Notification-area balloon (`Shell_NotifyIconW`) from a hidden message-only window, shown as the amux executable | Adds an amux icon to the notification area while the notifier is alive. Windows limits balloon titles to 63 and bodies to 255 UTF-16 units; text is cut between whole characters. If Windows refuses a notification, the icon and window are released so the next notification starts fresh. Needs no AppUserModelID, Start menu entry or installer. Type-checked for the Windows target only; not yet run on native Windows. |
| macOS | Compiles through `notify-rust` | Not a current target; untested |

## Other limits

- An OS call that never returns cannot be cancelled. It keeps one attempt
  thread busy, and later attempts report `Failed` until it returns.
- Up to 16 unread outcomes are kept; older unread outcomes are discarded.
- Attempt IDs are local to one `Notifier`. There is no deduplication across
  restarts; callers decide what to notify.

## Evidence

```sh
cargo test --locked --test notifications_contract          # bounds, isolation, deadline, no retry
cargo test --locked --test native_notification_contract    # Linux: real backend, missing bus
```

The native test points D-Bus at a nonexistent bus, so no notification appears
on the developer's desktop. It shows the real Linux backend reports a missing
service as `Failed` without blocking or panicking. No test shows a real
notification; that needs a native desktop check on each platform.
