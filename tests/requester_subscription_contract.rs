use amux::{
    delegation::*, delegation_queue::Queue, identity::Provider, task_routing::SessionTarget,
};
use serde_json::json;
use uuid::Uuid;

fn target(number: u64) -> Target {
    let origin = Origin {
        session_id: Uuid::new_v4(),
        instance_id: Uuid::new_v4(),
        provider: Provider::Codex,
        os: "linux".into(),
    };
    Target {
        workspace: Uuid::new_v4(),
        terminal: number,
        session: origin.session_id,
        agent_id: Some(format!("peer-{number}")),
        pid: number as u32 + 100,
        generation: format!("process-{number}"),
        directory: std::env::current_dir().unwrap(),
        project: "amux".into(),
        origin: Some(origin),
        goal: None,
    }
}

#[test]
fn informational_returns_can_reach_requester_during_user_review_without_resuming_work() {
    let a = target(1);
    let c = target(3);
    let d = target(4);
    let (_dir, mut queue) = fixture();
    let mut result = event(1, &a, &c, Kind::Commented);
    result.summary.as_mut().unwrap().state = "user_review".into();
    queue
        .receive(result, &[a.clone(), c.clone(), d.clone()], true, 1_000_000)
        .unwrap();
    queue.refresh(&[a.clone(), c, d], true, 1_000_000).unwrap();
    let attempt = queue
        .next(&std::collections::HashSet::from([a.session]))
        .unwrap();
    assert_eq!(attempt.target.session, a.session);
    assert!(
        attempt
            .event
            .recipient_terminal_text(&a, "A")
            .contains("Do not claim, reopen, or implement")
    );
    let mut created = attempt.event.clone();
    created.id = 2;
    created.kind = Kind::Created;
    assert!(!a.requester_result(&created));
    queue
        .receive(created, std::slice::from_ref(&a), true, 1_000_000)
        .unwrap();
    assert_eq!(queue.journal.receipts[1].outcome, Outcome::Skipped);
}
fn subscription(target: &Target) -> SessionTarget {
    SessionTarget {
        origin: target.origin.clone().unwrap(),
        generation: target.executor_generation(),
    }
}
fn event(id: i64, requester: &Target, owner: &Target, kind: Kind) -> Event {
    Event::parse(&json!({"id":id,"project_ident":"amux", "created_at":1_000_000,
        "kind":if kind == Kind::Commented {"task_commented"} else {"task_completed"},
        "origin":owner.origin, "comment":{"author":"executor","content":"Bounded review result"},
        "task":{"id":Uuid::new_v4(),"project_ident":"amux","status":if kind==Kind::Commented {"in_progress"} else {"done"},
        "title":"Requested review", "requester":subscription(requester),"owner_origin":owner.origin,"owner_agent_id":owner.agent_id}
    })).unwrap().unwrap()
}
fn fixture() -> (tempfile::TempDir, Queue) {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&root).unwrap();
    let dir = tempfile::tempdir_in(root).unwrap();
    let mut queue = Queue::open(dir.path().join("requester.json"), "fixture").unwrap();
    queue.subscribed(0, 0).unwrap();
    (dir, queue)
}

#[test]
fn requester_a_gets_comments_and_completion_from_reassigned_c_without_notifying_d_or_c() {
    let a = target(1);
    let b = target(2);
    let c = target(3);
    let d = target(4);
    let (_dir, mut queue) = fixture();
    for (id, kind) in [(1, Kind::Commented), (2, Kind::Completed)] {
        let mut result = event(id, &a, &b, kind);
        let saved = result.requester.clone();
        // Executor reassignment changes no requester metadata.
        result.owner = c.origin.clone();
        result.owner_agent_id = c.agent_id.clone().map(String::into_boxed_str);
        result.origin = c.origin.clone();
        assert_eq!(result.requester, saved);
        assert!(a.accepts(&result));
        assert!(!b.accepts(&result));
        assert!(!c.accepts(&result));
        assert!(!d.accepts(&result));
        queue
            .receive(
                result.clone(),
                &[a.clone(), c.clone(), d.clone()],
                true,
                1_000_000,
            )
            .unwrap();
        let attempt = queue
            .next(&std::collections::HashSet::from([
                a.session, c.session, d.session,
            ]))
            .unwrap();
        assert_eq!(attempt.target, a);
        let text = result.recipient_terminal_text(&a, "Requester A");
        assert!(
            text.contains("Requested task result")
                && text.contains("Informational return notice only")
        );
        assert!(text.contains("Do not claim, reopen, or implement"));
        queue.begin(&attempt).unwrap();
        queue.finish(&attempt, Outcome::Submitted).unwrap();
        queue
            .receive(result, &[a.clone(), c.clone(), d.clone()], true, 1_000_000)
            .unwrap();
        assert!(
            queue
                .next(&std::collections::HashSet::from([
                    a.session, c.session, d.session
                ]))
                .is_none()
        );
    }
    assert_eq!(queue.journal.receipts.len(), 2);
}

#[test]
fn no_implicit_requester_from_reporter_origin_or_reused_names_and_no_replacement_generation() {
    let a = target(1);
    let c = target(3);
    let result = event(1, &a, &c, Kind::Completed);
    let mut replacement = a.clone();
    replacement.generation.push_str("-restarted");
    assert!(!replacement.accepts(&result));
    let mut same_name = target(5);
    same_name.agent_id = a.agent_id.clone();
    assert!(!same_name.accepts(&result));
    let mut no_watcher = result.clone();
    no_watcher.requester = None;
    assert!(!a.accepts(&no_watcher));
    let mut own = result.clone();
    own.origin = a.origin.clone();
    assert!(!a.accepts(&own));
    let mut other_project = a.clone();
    other_project.project = "unrelated".into();
    assert!(!other_project.accepts(&result));
    let mut wrong_os = result.clone();
    wrong_os.required_os = Some(amux::task_platform::TaskOs::Windows);
    assert!(!a.accepts(&wrong_os));
}

#[test]
fn explicit_return_subscription_does_not_turn_unowned_results_into_broadcasts() {
    let a = target(1);
    let c = target(3);
    let d = target(4);
    let mut result = event(1, &a, &c, Kind::Completed);
    result.owner = None;
    result.owner_agent_id = None;
    result.origin = None;
    assert!(a.accepts(&result));
    assert!(!c.accepts(&result) && !d.accepts(&result));
    result.owner_agent_id = a.agent_id.clone().map(String::into_boxed_str);
    assert!(a.requester_result(&result));
    let mut reused_name = d;
    reused_name.agent_id = a.agent_id.clone();
    assert!(!reused_name.accepts(&result));
}

#[tokio::test]
async fn current_task_read_rejects_removed_replaced_and_os_retagged_watchers() {
    use std::io::{Read, Write};
    let a = target(1);
    let c = target(3);
    let event = event(1, &a, &c, Kind::Completed);
    let original = json!({"id":event.task,"project_ident":"amux","status":"done","requester":subscription(&a)});
    let mut removed = original.clone();
    removed["requester"] = serde_json::Value::Null;
    let mut replaced = original.clone();
    replaced["requester"]["generation"] = json!(Uuid::new_v4());
    let mut retagged = original.clone();
    retagged["labels"] = json!(["os:windows"]);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let task = event.task;
    let server = std::thread::spawn(move || {
        for value in [original, removed, replaced, retagged] {
            let (mut socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(std::time::Duration::from_secs(10)))
                .unwrap();
            let mut headers = Vec::new();
            while !headers.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).unwrap();
                headers.push(byte[0]);
            }
            assert!(
                String::from_utf8(headers)
                    .unwrap()
                    .starts_with(&format!("GET /v1/projects/amux/tasks/{task} "))
            );
            let body = value.to_string();
            write!(socket,"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",body.len()).unwrap();
        }
    });
    let gateway = amux::gateway::Gateway::new(&url, "fixture-only".into()).unwrap();
    for expected in [true, false, false, false] {
        assert_eq!(
            gateway.requester_is_current(&event, &a).await.unwrap(),
            expected
        );
    }
    server.join().unwrap();
}

#[test]
fn requester_reconnect_expiry_blocked_and_uncertain_receipts_never_replay() {
    let a = target(1);
    let c = target(3);
    let (dir, mut queue) = fixture();
    let result = event(1, &a, &c, Kind::Completed);
    queue
        .receive(result.clone(), std::slice::from_ref(&a), true, 1_000_000)
        .unwrap();
    assert!(queue.next(&std::collections::HashSet::new()).is_none());
    queue
        .refresh(
            std::slice::from_ref(&a),
            true,
            1_000_000 + MAX_EVENT_AGE_MS + 1,
        )
        .unwrap();
    assert!(queue.journal.receipts[0].recipients[0].outcome.terminal());
    let second = event(2, &a, &c, Kind::Completed);
    queue
        .receive(second.clone(), std::slice::from_ref(&a), true, 1_000_000)
        .unwrap();
    let attempt = queue
        .next(&std::collections::HashSet::from([a.session]))
        .unwrap();
    queue.begin(&attempt).unwrap();
    let mut reloaded = Queue::open(dir.path().join("requester.json"), "fixture").unwrap();
    assert_eq!(
        reloaded.journal.receipts[1].recipients[0].outcome,
        Outcome::Uncertain
    );
    reloaded.subscribed(2, 2).unwrap();
    reloaded
        .receive(second, std::slice::from_ref(&a), true, 1_000_000)
        .unwrap();
    assert!(
        reloaded
            .next(&std::collections::HashSet::from([a.session]))
            .is_none()
    );
    let absent = event(3, &a, &c, Kind::Completed);
    reloaded.receive(absent, &[], true, 1_000_000).unwrap();
    reloaded
        .refresh(std::slice::from_ref(&a), true, 1_000_000)
        .unwrap();
    assert!(
        reloaded
            .next(&std::collections::HashSet::from([a.session]))
            .is_none()
    );
}
