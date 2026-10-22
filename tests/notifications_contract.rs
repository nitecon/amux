//! Local notification delivery is bounded and isolated from its caller.
use amux::notifications::{Backend, Dropped, MAX_BODY, MAX_TITLE, Notifier, Outcome, QUEUE};
use std::{
    sync::{Arc, Mutex, mpsc},
    time::{Duration, Instant},
};

type Shown = Arc<Mutex<Vec<(String, String)>>>;

struct Record(Shown);
impl Backend for Record {
    fn show(&mut self, title: &str, body: &str) -> Result<(), String> {
        self.0.lock().unwrap().push((title.into(), body.into()));
        Ok(())
    }
}

fn wait_for(notifier: &Notifier, count: usize) -> Vec<Outcome> {
    let deadline = Instant::now() + Duration::from_secs(5);
    let mut outcomes = Vec::new();
    while outcomes.len() < count {
        outcomes.extend(notifier.outcomes());
        assert!(Instant::now() < deadline, "outcomes: {outcomes:?}");
        std::thread::sleep(Duration::from_millis(5));
    }
    outcomes
}

#[test]
fn payloads_are_bounded_and_cleaned() {
    let shown = Shown::default();
    let mut notifier = Notifier::new(Record(shown.clone()));
    assert_eq!(notifier.notify("  \n\t ", "body"), Err(Dropped::EmptyTitle));
    let id = notifier
        .notify(
            &format!("Task\nready\x07 {}", "t".repeat(500)),
            &format!("line one\nline two\x1b[31m {}", "b".repeat(5000)),
        )
        .unwrap();
    assert_eq!(wait_for(&notifier, 1), vec![Outcome::Shown { id }]);
    let shown = shown.lock().unwrap();
    assert_eq!(shown.len(), 1, "the empty title was never sent");
    let (title, body) = &shown[0];
    assert_eq!(title.chars().count(), MAX_TITLE);
    assert!(
        title.starts_with("Task ready tt") && title.ends_with('…'),
        "{title}"
    );
    assert_eq!(body.chars().count(), MAX_BODY);
    assert!(body.starts_with("line one\nline two[31m b"), "{body}");
    assert!(
        !title
            .chars()
            .chain(body.chars())
            .any(|c| c.is_control() && c != '\n')
    );
}

#[test]
fn a_stalled_service_never_blocks_the_caller() {
    struct Stalled(mpsc::Receiver<()>);
    impl Backend for Stalled {
        fn show(&mut self, _: &str, _: &str) -> Result<(), String> {
            let _ = self.0.recv();
            Ok(())
        }
    }
    let (release, wait) = mpsc::channel();
    let mut notifier = Notifier::new(Stalled(wait));
    let started = Instant::now();
    let mut accepted = 0;
    let mut dropped = 0;
    for i in 0..QUEUE * 3 {
        match notifier.notify(&format!("n{i}"), "") {
            Ok(_) => accepted += 1,
            Err(Dropped::QueueFull) => dropped += 1,
            Err(other) => panic!("{other:?}"),
        }
    }
    assert!(
        started.elapsed() < Duration::from_millis(500),
        "caller blocked"
    );
    // One attempt may be in flight beyond the queue; the rest are refused.
    assert!(
        (QUEUE..=QUEUE + 1).contains(&accepted),
        "accepted {accepted}"
    );
    assert_eq!(accepted + dropped, QUEUE * 3);
    for _ in 0..accepted {
        release.send(()).unwrap();
    }
    let outcomes = wait_for(&notifier, accepted);
    assert!(outcomes.iter().all(|o| matches!(o, Outcome::Shown { .. })));
}

#[test]
fn failures_and_panics_are_reported_once_without_retry() {
    struct Flaky(Arc<Mutex<u32>>);
    impl Backend for Flaky {
        fn show(&mut self, title: &str, _: &str) -> Result<(), String> {
            *self.0.lock().unwrap() += 1;
            match title {
                "fail" => Err("no notification service".into()),
                "panic" => panic!("backend bug"),
                _ => Ok(()),
            }
        }
    }
    let calls = Arc::new(Mutex::new(0));
    let mut notifier = Notifier::new(Flaky(calls.clone()));
    let fail = notifier.notify("fail", "").unwrap();
    let panic = notifier.notify("panic", "").unwrap();
    let ok = notifier.notify("ok", "").unwrap();
    assert_eq!(
        wait_for(&notifier, 3),
        vec![
            Outcome::Failed {
                id: fail,
                reason: "no notification service".into()
            },
            Outcome::Failed {
                id: panic,
                reason: "notification backend panicked".into()
            },
            Outcome::Shown { id: ok },
        ]
    );
    assert_eq!(*calls.lock().unwrap(), 3, "each attempt runs exactly once");
}
