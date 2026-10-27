use amux::{delegation::*, identity::Provider, task_routing::SessionTarget};
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
