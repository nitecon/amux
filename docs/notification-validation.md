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
for outcome in notifier.outcomes() { /* Shown { id } or Failed { id, reason } */ }
```

- `notify` queues one attempt and returns its ID immediately. It returns
  `Dropped::QueueFull` when 16 attempts are already waiting, `Dropped::EmptyTitle`
  for a blank title, and `Dropped::WorkerStopped` if the worker has exited.
- One worker thread runs every attempt in order. A missing, slow, failing or
  panicking notification service cannot block the caller, terminals or task
  operations.
- Each attempt runs once. Failures are reported, never retried.
- `Shown` means the OS notification service accepted the request. It does not
  mean the user saw it, and no outcome ever means a task was received, claimed
  or started.
- Titles are limited to 120 characters and bodies to 1,000, with a `…` where text
  was cut. Control characters are removed; bodies keep line breaks.

## Platforms

| Platform | Backend | Known limits |
| --- | --- | --- |
| Linux | freedesktop notification service over the D-Bus session bus (`notify-rust`, pure Rust) | Needs a running notification daemon; without one, attempts report `Failed` |
| Windows | Toast notification (`notify-rust` via `tauri-winrt-notification`) | Toasts appear under **Windows PowerShell**, not amux. A portable executable has no registered AppUserModelID. Showing "amux" needs a registered ID, normally from a Start menu shortcut created by an installer, which is outside the current portable packaging. Not yet run on native Windows. |
| macOS | Compiles through `notify-rust` | Not a current target; untested |

## Other limits

- An OS call that never returns cannot be cancelled. Later attempts wait behind
  it until the queue fills, and then further attempts are dropped and reported.
- Up to 16 unread outcomes are kept; older unread outcomes are discarded.
- Attempt IDs are local to one `Notifier`. There is no deduplication across
  restarts; callers decide what to notify.

## Evidence

```sh
cargo test --locked --test notifications_contract          # bounds, isolation, no retry
cargo test --locked --test native_notification_contract    # Linux: real backend, missing bus
```

The native test points D-Bus at a nonexistent bus, so no notification appears
on the developer's desktop. It shows the real Linux backend reports a missing
service as `Failed` without blocking or panicking. No test shows a real
notification; that needs a native desktop check on each platform.
