use amux::{delegation::*, delegation_queue::Queue};
use std::collections::HashSet;
use uuid::Uuid;

fn target() -> Target {
    Target {
        workspace: Uuid::new_v4(),
        terminal: 1,
        session: Uuid::new_v4(),
        pid: 5,
        generation: "fixture".into(),
        directory: std::env::current_dir().unwrap(),
        project: "amux".into(),
        origin: None,
        goal: None,
    }
}
fn event(id: i64) -> Event {
    Event {
        id,
        project: "amux".into(),
        task: Uuid::new_v4(),
        kind: Kind::Created,
        created_at: 1_000_000,
        origin: None,
        goal: None,
    }
}
fn fixture() -> (tempfile::TempDir, Queue) {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&root).unwrap();
    let dir = tempfile::tempdir_in(root).unwrap();
    let mut queue = Queue::open(dir.path().join("journal.json"), "fixture").unwrap();
    queue.subscribed(0, 9).unwrap();
    (dir, queue)
}
#[test]
fn offline_history_duplicate_and_later_agents_never_receive_input() {
    let (_dir, mut queue) = fixture();
    let t = target();
    queue
        .receive(event(9), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    assert_eq!(queue.journal.receipts[0].status(), "skipped");
    queue
        .receive(event(10), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    queue
        .receive(event(10), &[target()], true, 1_000_000)
        .unwrap();
    assert_eq!(queue.journal.receipts.len(), 2);
    let later = target();
    queue
        .refresh(std::slice::from_ref(&later), true, 1_000_000)
        .unwrap();
    assert!(
        queue
            .next(&HashSet::from([later.session, t.session]))
            .is_none()
    );
    assert_eq!(queue.journal.receipts[1].status(), "skipped");
}
#[test]
fn busy_peer_does_not_block_other_peer_but_each_peer_keeps_fifo() {
    let (_dir, mut queue) = fixture();
    let first = target();
    let mut second = target();
    second.terminal = 2;
    let targets = [first.clone(), second.clone()];
    queue.receive(event(10), &targets, true, 1_000_000).unwrap();
    queue.receive(event(11), &targets, true, 1_000_000).unwrap();
    let ready = HashSet::from([second.session]);
    let attempt = queue.next(&ready).unwrap();
    assert_eq!(attempt.event.id, 10);
    assert_eq!(attempt.target, second);
    queue.begin(&attempt).unwrap();
    queue.finish(&attempt, Outcome::Submitted).unwrap();
    let attempt = queue.next(&ready).unwrap();
    assert_eq!(attempt.event.id, 11);
    let first_attempt = queue.next(&HashSet::from([first.session])).unwrap();
    assert_eq!(first_attempt.event.id, 10);
}
#[test]
fn durable_attempt_precedes_input_and_restart_never_retries() {
    let (dir, mut queue) = fixture();
    let t = target();
    queue
        .receive(event(10), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    let attempt = queue.next(&HashSet::from([t.session])).unwrap();
    queue.begin(&attempt).unwrap();
    let after_crash = Queue::open(dir.path().join("journal.json"), "fixture").unwrap();
    assert_eq!(after_crash.journal.receipts[0].status(), "uncertain");
    assert!(after_crash.next(&HashSet::from([t.session])).is_none());
    queue.finish(&attempt, Outcome::Queued).unwrap();
    let deferred = queue.next(&HashSet::from([t.session])).unwrap();
    queue.begin(&deferred).unwrap();
    queue.finish(&deferred, Outcome::Uncertain).unwrap();
    assert!(queue.next(&HashSet::from([t.session])).is_none());
}
#[test]
fn unresolved_metadata_remains_pinned_and_consent_withdrawal_retires_it() {
    let (_dir, mut queue) = fixture();
    let mut t = target();
    t.project.clear();
    queue
        .receive(event(10), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    assert!(queue.next(&HashSet::from([t.session])).is_none());
    t.project = "amux".into();
    queue
        .refresh(std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    assert!(queue.next(&HashSet::from([t.session])).is_some());
    queue
        .refresh(std::slice::from_ref(&t), false, 1_000_000)
        .unwrap();
    queue
        .refresh(std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    assert!(queue.next(&HashSet::from([t.session])).is_none());
}
#[test]
fn delayed_recorded_cannot_confirm_a_changed_recipient_snapshot() {
    let (_dir, mut queue) = fixture();
    let t = target();
    queue
        .receive(event(10), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    assert_eq!(queue.acknowledgments()[0]["status"], "queued");
    assert!(queue.acknowledgments().is_empty());
    let attempt = queue.next(&HashSet::from([t.session])).unwrap();
    queue.begin(&attempt).unwrap();
    queue.finish(&attempt, Outcome::Submitted).unwrap();
    queue.recorded(10, "queued").unwrap();
    assert!(!queue.journal.receipts[0].confirmed);
    assert_eq!(queue.acknowledgments()[0]["status"], "injected");
    queue.recorded(10, "injected").unwrap();
    assert!(queue.journal.receipts[0].confirmed);
    assert_eq!(queue.journal.cursor, Some(10));
}
#[test]
fn server_terminal_fence_wins_and_disk_failure_cannot_authorize_input() {
    let (dir, mut queue) = fixture();
    let t = target();
    queue
        .receive(event(10), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    queue.acknowledgments();
    queue.recorded(10, "skipped").unwrap();
    assert!(queue.next(&HashSet::from([t.session])).is_none());
    queue
        .receive(event(11), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    let attempt = queue.next(&HashSet::from([t.session])).unwrap();
    std::fs::remove_file(dir.path().join("journal.json")).unwrap();
    std::fs::create_dir(dir.path().join("journal.json")).unwrap();
    assert!(queue.begin(&attempt).is_err());
    assert_eq!(queue.journal.receipts[0].status(), "queued");
}

#[test]
fn newly_bound_self_origin_retires_only_its_own_copy_and_ungrouped_goals_can_change() {
    let (_dir, mut queue) = fixture();
    let mut own = target();
    let peer = target();
    let origin = Origin {
        session_id: Uuid::new_v4(),
        instance_id: Uuid::new_v4(),
        provider: amux::identity::Provider::Codex,
        os: "linux".into(),
    };
    let mut e = event(10);
    e.origin = Some(origin.clone());
    queue
        .receive(e, &[own.clone(), peer.clone()], true, 1_000_000)
        .unwrap();
    own.origin = Some(origin.clone());
    queue
        .refresh(&[own.clone(), peer.clone()], true, 1_000_000)
        .unwrap();
    assert!(queue.next(&HashSet::from([own.session])).is_none());
    assert_eq!(
        queue.next(&HashSet::from([peer.session])).unwrap().target,
        peer
    );
    let mut other = target();
    other.origin = Some(origin);
    other.goal = Some("first".into());
    queue
        .receive(event(11), std::slice::from_ref(&other), true, 1_000_000)
        .unwrap();
    other.goal = Some("second".into());
    queue
        .refresh(&[own, peer, other.clone()], true, 1_000_000)
        .unwrap();
    assert_eq!(
        queue.next(&HashSet::from([other.session])).unwrap().target,
        other
    );
}
