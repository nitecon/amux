use amux::{delegation::*, delegation_queue::Queue};
use std::collections::HashSet;
use uuid::Uuid;

fn target() -> Target {
    Target {
        workspace: Uuid::new_v4(),
        terminal: 1,
        session: Uuid::new_v4(),
        agent_id: None,
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
        routing: Default::default(),
        owner: None,
        owner_agent_id: None,
        requester: None,
        required_os: None,
        summary: None,
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
fn os_retag_retires_pending_notification_before_native_input() {
    let (_dir, mut queue) = fixture();
    let t = target();
    let original = event(10);
    queue
        .receive(original.clone(), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    let attempt = queue.next(&HashSet::from([t.session])).unwrap();
    queue.begin(&attempt).unwrap();
    queue
        .reconcile_backlog(
            "amux",
            &[serde_json::json!({"id":original.task,"labels":["os:windows"]})],
        )
        .unwrap();
    assert!(!queue.submitting(&attempt));
    assert_eq!(
        queue.journal.receipts[0].recipients[0].outcome,
        Outcome::Uncertain
    );
    let windows = Event::parse(&serde_json::json!({"id":11,"kind":"task_created","project_ident":"amux","created_at":1_000_000,"task":{"id":Uuid::new_v4(),"labels":["os:windows"]}})).unwrap().unwrap();
    assert!(
        !t.accepts(&windows),
        "unknown OS cannot receive OS-restricted work"
    );
    assert!(windows.terminal_text().contains("OS: windows"));
}

#[test]
fn user_review_keeps_bubbles_but_retires_old_input_and_allows_new_todo_work() {
    let (_dir, mut queue) = fixture();
    let t = target();
    let original = event(10);
    queue
        .receive(original.clone(), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    let ready = HashSet::from([t.session]);
    assert!(queue.next(&ready).is_some());
    let review = Event::parse(&serde_json::json!({"id":11,"kind":"task_commented","project_ident":"amux","created_at":1_000_000,
        "task":{"id":original.task,"status":"user_review","title":"Needs user decision"},
        "comment":{"author":"User","content":"I will decide later"}})).unwrap().unwrap();
    assert_eq!(review.state(), "user_review");
    assert!(!t.accepts(&review));
    queue
        .receive(review.clone(), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    assert!(queue.next(&ready).is_none());
    assert!(
        queue
            .journal
            .recent
            .last()
            .unwrap()
            .terminal_text()
            .contains("State: user_review")
    );
    for _ in 0..3 {
        queue
            .refresh(std::slice::from_ref(&t), true, 1_000_000)
            .unwrap();
        queue
            .reconcile_backlog(
                "amux",
                &[serde_json::json!({"id":original.task,"status":"user_review"})],
            )
            .unwrap();
        assert!(queue.next(&ready).is_none());
    }
    let mut returned = event(12);
    returned.task = original.task;
    queue
        .receive(returned, std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    // A duplicate/delayed old review event cannot retire newer task events.
    queue
        .receive(review, std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    assert_eq!(queue.next(&ready).unwrap().event.id, 12);
}

#[test]
fn a_review_snapshot_cancels_an_already_verifying_notification() {
    let (_dir, mut queue) = fixture();
    let t = target();
    let original = event(10);
    queue
        .receive(original.clone(), std::slice::from_ref(&t), true, 1_000_000)
        .unwrap();
    let attempt = queue.next(&HashSet::from([t.session])).unwrap();
    queue.begin(&attempt).unwrap();
    queue
        .reconcile_backlog(
            "amux",
            &[serde_json::json!({"id":original.task,"status":"user_review"})],
        )
        .unwrap();
    assert!(!queue.submitting(&attempt));
    assert_eq!(
        queue.journal.receipts[0].recipients[0].outcome,
        Outcome::Uncertain
    );
    assert!(queue.next(&HashSet::from([t.session])).is_none());
}

#[test]
fn unassigned_todo_events_wait_for_idle_assignment_instead_of_notifying_peers() {
    let (_dir, mut queue) = fixture();
    let a = target();
    let b = target();
    for (i, kind) in ["task_created", "task_unclaimed", "task_commented"]
        .into_iter()
        .enumerate()
    {
        let event = Event::parse(
            &serde_json::json!({"id":10+i,"kind":kind,"project_ident":"amux","created_at":1_000_000,
            "task":{"id":Uuid::new_v4(),"status":"todo","title":"Queued work"},
            "comment":{"author":"Peer","content":"Still required"}}),
        )
        .unwrap()
        .unwrap();
        queue
            .receive(event, &[a.clone(), b.clone()], true, 1_000_000)
            .unwrap();
    }
    assert_eq!(
        queue.journal.recent.len(),
        3,
        "retain events for user bubbles"
    );
    assert!(
        queue
            .journal
            .receipts
            .iter()
            .all(|r| r.recipients.is_empty() && r.reason.contains("idle-agent assignment"))
    );
    assert!(queue.next(&HashSet::from([a.session, b.session])).is_none());

    let owned = Event::parse(&serde_json::json!({"id":13,"kind":"task_created","project_ident":"amux","created_at":1_000_000,
        "task":{"id":Uuid::new_v4(),"status":"todo","owner_agent_id":"assigned-agent"}})).unwrap().unwrap();
    let owner = Target {
        agent_id: Some("assigned-agent".into()),
        origin: Some(Origin {
            session_id: a.session,
            instance_id: Uuid::new_v4(),
            provider: amux::identity::Provider::Codex,
            os: "linux".into(),
        }),
        ..a
    };
    queue
        .receive(owned, &[owner.clone(), b.clone()], true, 1_000_000)
        .unwrap();
    assert_eq!(
        queue
            .next(&HashSet::from([owner.session, b.session]))
            .unwrap()
            .target
            .session,
        owner.session
    );
}

#[test]
fn explicit_routing_controls_native_audience_and_keeps_one_task() {
    use amux::{
        identity::Provider,
        task_routing::{Routing, SessionTarget},
    };
    let origin = |session| Origin {
        session_id: session,
        instance_id: Uuid::new_v4(),
        provider: Provider::Codex,
        os: "windows".into(),
    };
    let mut a = target();
    a.origin = Some(origin(a.session));
    let mut b = target();
    b.origin = Some(origin(b.session));
    let selected = SessionTarget {
        origin: a.origin.clone().unwrap(),
        generation: a.executor_generation(),
    };
    let (_dir, mut queue) = fixture();
    let mut exact = event(10);
    *exact.routing = Routing::Session {
        target: selected.clone(),
    };
    exact.owner = a.origin.clone();
    queue
        .receive(exact.clone(), &[a.clone(), b.clone()], true, 1_000_000)
        .unwrap();
    assert_eq!(
        queue.journal.receipts[0].recipients[0].outcome,
        Outcome::Queued
    );
    assert_eq!(
        queue.journal.receipts[0].recipients[1].outcome,
        Outcome::Skipped
    );
    let mut queued = event(11);
    *queued.routing = Routing::Unassigned;
    queue
        .receive(queued, &[a.clone(), b.clone()], true, 1_000_000)
        .unwrap();
    assert!(
        queue.journal.receipts[1]
            .recipients
            .iter()
            .all(|r| r.outcome == Outcome::Skipped)
    );
    let mut broadcast = event(12);
    *broadcast.routing = Routing::Broadcast;
    queue
        .receive(broadcast, &[a.clone(), b.clone()], true, 1_000_000)
        .unwrap();
    assert!(
        queue.journal.receipts[2]
            .recipients
            .iter()
            .all(|r| r.outcome == Outcome::Queued)
    );
    let mut replacement = a.clone();
    replacement.generation = "new process".into();
    assert!(!replacement.accepts(&exact));
    let mut later = exact.clone();
    later.kind = Kind::Commented;
    later.owner = b.origin.clone();
    assert!(!a.accepts(&later));
    assert!(b.accepts(&later));
    let released=Event::parse(&serde_json::json!({"id":13,"kind":"task_unclaimed","project_ident":"amux","created_at":1_000_000,"task":{"id":exact.task,"routing":exact.routing,"owner_origin":null}})).unwrap().unwrap();
    assert!(a.accepts(&released));
    assert!(b.accepts(&released));
    let foreign = Target {
        project: "other".into(),
        ..b
    };
    assert!(!foreign.accepts(&event(14)));
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

#[test]
fn wire_assignment_filters_created_comments_and_completion_by_owner() {
    use amux::{identity::Provider, task_routing::Routing};
    use serde_json::json;
    let mut a = target();
    let mut b = target();
    for (target, name) in [
        (&mut a, "Gene Amdahl - Codex"),
        (&mut b, "Grace Hopper - Claude"),
    ] {
        target.agent_id = Some(name.into());
        target.origin = Some(Origin {
            session_id: target.session,
            instance_id: Uuid::new_v4(),
            provider: Provider::Codex,
            os: "linux".into(),
        });
    }
    let (_dir, mut queue) = fixture();
    for (index, kind) in ["task_created", "task_commented", "task_completed"]
        .into_iter()
        .enumerate()
    {
        let id = 10 + index as i64;
        let wire = json!({"id":id,"kind":kind,"project_ident":"amux","created_at":1_000_000,
            "task":{"id":Uuid::new_v4(),"owner_agent_id":a.agent_id},
            "comment":{"author":"human"}});
        let event = Event::parse(&wire).unwrap().unwrap();
        queue
            .receive(event, &[a.clone(), b.clone()], true, 1_000_000)
            .unwrap();
        let recipients = &queue.journal.receipts[index].recipients;
        assert_eq!(recipients[0].outcome, Outcome::Queued, "{kind}");
        assert_eq!(recipients[1].outcome, Outcome::Skipped, "{kind}");
    }
    // Null/missing ownership is genuinely unassigned and still reaches peers.
    let mut unassigned = event(13);
    unassigned.kind = Kind::Commented;
    assert!(a.accepts(&unassigned) && b.accepts(&unassigned));
    // An exact execution always takes precedence over a reused display label.
    let mut exact = unassigned.clone();
    exact.owner = a.origin.clone();
    exact.owner_agent_id = b.agent_id.clone().map(String::into_boxed_str);
    assert!(a.accepts(&exact));
    assert!(!b.accepts(&exact));
    let mut restarted = a.clone();
    restarted.origin.as_mut().unwrap().instance_id = Uuid::new_v4();
    assert!(!restarted.accepts(&exact));
    // Broadcast routing cannot override a current owner.
    exact.kind = Kind::Created;
    *exact.routing = Routing::Broadcast;
    assert!(!b.accepts(&exact));
    // The submitting owner already knows about its own creation/comment.
    exact.origin = a.origin.clone();
    assert!(!a.accepts(&exact));
    // An owner label unknown locally must never be treated as unassigned.
    let mut unknown = unassigned;
    unknown.owner_agent_id = Some("remote-agent".into());
    assert!(!a.accepts(&unknown) && !b.accepts(&unknown));
    for malformed in [json!(42), json!(""), json!("bad\nidentity")] {
        let wire = json!({"id":99,"kind":"task_commented","project_ident":"amux","created_at":1_000_000,
            "task":{"id":Uuid::new_v4(),"owner_agent_id":malformed},"comment":{"author":"human"}});
        assert!(Event::parse(&wire).is_err());
    }
}

#[test]
fn recent_event_bubbles_survive_ack_pruning_and_remain_bounded_across_restart() {
    let (dir, mut queue) = fixture();
    for id in 10..80 {
        queue.receive(event(id), &[], false, 1_000_000).unwrap();
        queue.recorded(id, "skipped").unwrap();
    }
    assert!(queue.journal.receipts.len() <= 1);
    assert_eq!(queue.journal.recent.len(), MAX_RECENT_EVENTS);
    assert_eq!(queue.journal.recent.first().unwrap().id, 30);
    assert_eq!(queue.journal.recent.last().unwrap().id, 79);
    drop(queue);
    let queue = Queue::open(dir.path().join("journal.json"), "fixture").unwrap();
    assert_eq!(queue.journal.recent.len(), MAX_RECENT_EVENTS);
}
