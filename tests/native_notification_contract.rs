//! The real Linux backend reports a missing notification service as a failed
//! attempt instead of blocking or panicking. Points D-Bus at a nonexistent bus
//! so no notification appears on a developer's desktop.
#![cfg(all(target_os = "linux", feature = "desktop"))]
use amux::notifications::{Notifier, Outcome};
use std::time::{Duration, Instant};

#[test]
fn missing_notification_service_is_a_failed_attempt() {
    // SAFETY: this test binary has no other threads reading the environment yet.
    unsafe {
        std::env::set_var(
            "DBUS_SESSION_BUS_ADDRESS",
            "unix:path=/nonexistent/amux-test-bus",
        );
    }
    let mut notifier = Notifier::native();
    let started = Instant::now();
    let id = notifier.notify("amux test", "not delivered").unwrap();
    assert!(
        started.elapsed() < Duration::from_millis(100),
        "caller blocked"
    );
    let deadline = Instant::now() + Duration::from_secs(10);
    let outcome = loop {
        if let Some(outcome) = notifier.outcomes().pop() {
            break outcome;
        }
        assert!(Instant::now() < deadline, "no outcome");
        std::thread::sleep(Duration::from_millis(10));
    };
    assert!(
        matches!(&outcome, Outcome::Failed { id: got, .. } if *got == id),
        "{outcome:?}"
    );
}
