//! Black-box gateway client contract: wire identity, claim acknowledgment,
//! response bounds and honest failure. Uses a local mock; no live gateway.
use amux::{
    gateway::{Gateway, TaskDetails},
    identity::{AgentIdentity, Provider},
};
use reqwest::Method;
use serde_json::{Value, json};
use std::{
    io::{Read, Write},
    net::TcpListener,
    path::Path,
    thread,
    time::{Duration, Instant},
};

const TASK: &str = "01a164c4-5378-7360-ac9a-66b176e315fb";
const LIMIT: usize = 4 * 1024 * 1024;

struct Request {
    head: String,
    body: Value,
}

/// Serves one raw HTTP response per expected request, then returns what it saw.
fn server(replies: Vec<Vec<u8>>) -> (String, thread::JoinHandle<Vec<Request>>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    listener.set_nonblocking(true).unwrap();
    let handle = thread::spawn(move || {
        let mut seen = Vec::new();
        for reply in replies {
            let deadline = Instant::now() + Duration::from_secs(15);
            let mut stream = loop {
                match listener.accept() {
                    Ok((stream, _)) => break stream,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        assert!(Instant::now() < deadline, "missing gateway request");
                        thread::sleep(Duration::from_millis(5));
                    }
                    Err(e) => panic!("accept: {e}"),
                }
            };
            // Windows accepted sockets can inherit the listener's nonblocking mode.
            stream.set_nonblocking(false).unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let mut bytes = Vec::new();
            let (end, length) = loop {
                let mut buf = [0; 4096];
                let count = stream.read(&mut buf).unwrap();
                assert_ne!(count, 0, "request closed before headers");
                bytes.extend_from_slice(&buf[..count]);
                if let Some(end) = bytes.windows(4).position(|w| w == b"\r\n\r\n") {
                    let head = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                    let length = head
                        .lines()
                        .find_map(|l| l.strip_prefix("content-length: ")?.parse().ok())
                        .unwrap_or(0);
                    break (end + 4, length);
                }
            };
            while bytes.len() < end + length {
                let mut buf = [0; 4096];
                let count = stream.read(&mut buf).unwrap();
                assert_ne!(count, 0, "incomplete request body");
                bytes.extend_from_slice(&buf[..count]);
            }
            seen.push(Request {
                head: String::from_utf8_lossy(&bytes[..end]).to_lowercase(),
                body: serde_json::from_slice(&bytes[end..end + length]).unwrap_or(Value::Null),
            });
            // The client may abort an oversized reply early; that is expected.
            let _ = stream.write_all(&reply);
        }
        seen
    });
    (url, handle)
}

fn reply(status: u16, body: &Value) -> Vec<u8> {
    let body = serde_json::to_vec(body).unwrap();
    let mut out = format!(
        "HTTP/1.1 {status} Test\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    )
    .into_bytes();
    out.extend(body);
    out
}

fn owned_by(actor: &AgentIdentity) -> Value {
    json!({
        "id": TASK,
        "project_ident": "demo",
        "status": "in_progress",
        "owner_origin": {
            "session_id": actor.session_id.to_string(),
            "instance_id": actor.instance_id.to_string(),
        }
    })
}

fn agent(provider: Provider) -> AgentIdentity {
    AgentIdentity::for_terminal(provider, Path::new(env!("CARGO_MANIFEST_DIR")), {
        static NUMBER: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
        NUMBER.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
    })
    .unwrap()
}

#[test]
fn requester_creation_keeps_executor_independent_and_checks_capability_before_post() {
    use amux::{delegation::Origin, task_routing::SessionTarget};
    let actor = agent(Provider::Codex);
    let requester = SessionTarget {
        origin: Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let executor = uuid::Uuid::new_v4();
    let accepted = json!({"id":TASK,"project_ident":"demo","status":"todo","requester":requester,"owner_agent_id":executor});
    let (url, mock) = server(vec![
        reply(200, &json!({"requester_subscriptions":1})),
        reply(200, &accepted),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let result = runtime
        .block_on(gateway.create_task_with_requester(
            "demo",
            json!({"title":"Review","specification":"literal\ncontext"}),
            &actor,
            &requester,
        ))
        .unwrap();
    assert_eq!(result["owner_agent_id"], json!(executor));
    let requests = mock.join().unwrap();
    assert!(requests[0].head.starts_with("get /v1/tasks/capabilities "));
    assert!(
        requests[1]
            .head
            .starts_with("post /v1/projects/demo/tasks ")
    );
    assert!(
        requests[1]
            .head
            .contains(&format!("x-agent-session-id: {}", actor.session_id))
    );
    assert_eq!(requests[1].body["requester"], json!(requester));
    assert_eq!(requests[1].body["specification"], "literal\ncontext");

    for capability in [json!({}), json!({"requester_subscriptions":0})] {
        let (url, mock) = server(vec![reply(200, &capability)]);
        let gateway = Gateway::new(&url, "test-only".into()).unwrap();
        assert!(
            runtime
                .block_on(gateway.create_task_with_requester(
                    "demo",
                    json!({"title":"Review"}),
                    &actor,
                    &requester
                ))
                .is_err()
        );
        assert_eq!(
            mock.join().unwrap().len(),
            1,
            "old gateway must never receive a create"
        );
    }
    let mut missing_ack = accepted.clone();
    missing_ack.as_object_mut().unwrap().remove("requester");
    let (url, mock) = server(vec![
        reply(200, &json!({"requester_subscriptions":1})),
        reply(200, &missing_ack),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let error = runtime
        .block_on(gateway.create_task_with_requester(
            "demo",
            json!({"title":"Review"}),
            &actor,
            &requester,
        ))
        .unwrap_err();
    assert!(
        error
            .to_string()
            .contains("inspect the task before retrying")
    );
    assert_eq!(
        mock.join().unwrap().len(),
        2,
        "unconfirmed create is never retried"
    );
}

#[test]
fn human_requester_set_clear_require_exact_resource_and_do_not_impersonate_watcher() {
    use amux::{delegation::Origin, task_routing::SessionTarget};
    let watcher = SessionTarget {
        origin: Origin {
            session_id: uuid::Uuid::new_v4(),
            instance_id: uuid::Uuid::new_v4(),
            provider: Provider::Claude,
            os: "windows".into(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for requested in [Some(&watcher), None] {
        let accepted =
            json!({"id":TASK,"project_ident":"demo","status":"todo","requester":requested});
        let (url, mock) = server(vec![
            reply(200, &json!({"requester_subscriptions":1})),
            reply(200, &accepted),
        ]);
        let gateway = Gateway::new(&url, "test-only".into()).unwrap();
        runtime
            .block_on(gateway.set_task_requester("demo", TASK.parse().unwrap(), requested, None))
            .unwrap();
        let requests = mock.join().unwrap();
        assert!(
            requests[1]
                .head
                .starts_with(&format!("put /v1/projects/demo/tasks/{TASK}/requester "))
        );
        assert_eq!(requests[1].body, json!({"requester":requested}));
        assert!(
            !requests[1].head.contains("x-agent-"),
            "human never impersonates selected watcher"
        );
    }
    for accepted in [
        json!({"id":TASK,"project_ident":"other","requester":watcher}),
        json!({"id":uuid::Uuid::new_v4(),"project_ident":"demo","requester":watcher}),
        json!({"id":TASK,"project_ident":"demo","requester":null}),
        json!({"id":TASK,"project_ident":"demo"}),
    ] {
        let (url, mock) = server(vec![
            reply(200, &json!({"requester_subscriptions":1})),
            reply(200, &accepted),
        ]);
        let gateway = Gateway::new(&url, "test-only".into()).unwrap();
        assert!(
            runtime
                .block_on(gateway.set_task_requester(
                    "demo",
                    TASK.parse().unwrap(),
                    Some(&watcher),
                    None
                ))
                .is_err()
        );
        assert_eq!(mock.join().unwrap().len(), 2);
    }
}

#[test]
fn cli_handoff_uses_callers_subscription_and_a_different_exact_executor() {
    use amux::{delegation::Origin, identity::AgentRecord, task_routing::SessionTarget};
    let actor = agent(Provider::Codex);
    let requester = SessionTarget {
        origin: Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let executor = SessionTarget {
        origin: Origin {
            session_id: uuid::Uuid::new_v4(),
            instance_id: uuid::Uuid::new_v4(),
            provider: Provider::Claude,
            os: "windows".into(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64;
    let directory = json!({"canonical_remote":"github.com/nitecon/amux","targets":[
        {"target":requester,"seen_at":now,"expires_at":now+120000},
        {"target":executor,"seen_at":now,"expires_at":now+120000}
    ]});
    let projects = json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]);
    let routing = json!({"mode":"session","target":executor});
    let accepted = json!({"id":TASK,"title":"Review","project_ident":"demo","status":"todo","requester":requester,"routing":routing,"owner_origin":executor.origin});
    let (url, mock) = server(vec![
        reply(200, &projects),
        reply(200, &directory),
        reply(200, &projects),
        reply(200, &directory),
        reply(200, &json!({"requester_subscriptions":1})),
        reply(200, &accepted),
    ]);
    let state = tempfile::tempdir().unwrap();
    AgentRecord::inspect(actor.clone(), std::process::id())
        .unwrap()
        .register_at(state.path())
        .unwrap();
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_amux"))
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .env("AMUX_STATE_DIR", state.path())
        .env("AMUX_GATEWAY_KEY", "test-only")
        .args([
            "--agent",
            &actor.session_id.to_string(),
            "tasks",
            "--project",
            "demo",
            "--gateway-url",
            &url,
            "create",
            "Review",
            "--to-agent",
            &executor.origin.session_id.to_string(),
        ])
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = mock.join().unwrap();
    assert_eq!(requests.len(), 6);
    assert_eq!(requests[5].body["requester"], json!(requester));
    assert_eq!(requests[5].body["routing"], routing);
    assert!(
        requests[5]
            .head
            .contains(&format!("x-agent-session-id: {}", actor.session_id))
    );
    assert!(!requests[5].head.contains(&format!(
        "x-agent-session-id: {}",
        executor.origin.session_id
    )));
    assert!(String::from_utf8_lossy(&output.stdout).contains("Result recipient"));
}

#[test]
fn cli_os_creation_retag_and_wrong_os_claim_use_authoritative_replies() {
    let base = Path::new(env!("CARGO_TARGET_TMPDIR")).join("os-gateway");
    std::fs::create_dir_all(&base).unwrap();
    let state = tempfile::tempdir_in(base).unwrap();
    let actor = agent(Provider::Codex);
    let record = amux::identity::AgentRecord {
        identity: actor.clone(),
        pid: 0,
        process_start: 0,
        process_generation: "unavailable".into(),
    };
    std::fs::create_dir(state.path().join("agents")).unwrap();
    std::fs::write(
        state
            .path()
            .join("agents")
            .join(format!("{}.json", actor.session_id)),
        serde_json::to_vec(&record).unwrap(),
    )
    .unwrap();
    let other = if actor.os == "windows" {
        "linux"
    } else {
        "windows"
    };
    let projects = json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]);
    let queued = json!({"id":TASK,"project_ident":"demo","status":"todo","labels":["autoreview-finding",format!("os:{other}")],"owner_agent_id":null,"owner_origin":null});
    let owned = json!({"id":TASK,"project_ident":"demo","status":"in_progress","labels":["keep"],"owner_origin":{"session_id":actor.session_id,"instance_id":actor.instance_id,"provider":actor.provider,"os":actor.os}});
    let released = json!({"id":TASK,"project_ident":"demo","status":"todo","labels":["keep",format!("os:{other}")],"owner_agent_id":null,"owner_origin":null,"started_at":null,"done_at":null,"routing":{"mode":"auto"}});
    let (url, server) = server(vec![
        reply(200, &projects),
        reply(200, &queued),
        reply(200, &projects),
        reply(200, &owned),
        reply(200, &released),
        reply(200, &queued),
    ]);
    let run = |args: &[&str]| {
        let mut cmd = std::process::Command::new(env!("CARGO_BIN_EXE_amux"));
        for name in actor.environment().keys() {
            cmd.env_remove(name);
        }
        cmd.current_dir(env!("CARGO_MANIFEST_DIR"))
            .env("AMUX_STATE_DIR", state.path())
            .env("AMUX_GATEWAY_KEY", "fixture")
            .args([
                "--agent",
                &actor.session_id.to_string(),
                "tasks",
                "--project",
                "demo",
                "--gateway-url",
                &url,
            ])
            .args(args)
            .output()
            .unwrap()
    };
    for args in [
        vec![
            "create",
            "Cross platform",
            "--os",
            other,
            "--review-finding",
        ],
        vec!["set-os", TASK, "--os", other],
    ] {
        let output = run(&args);
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
    }
    let refused = run(&["claim", TASK]);
    assert!(!refused.status.success());
    assert!(String::from_utf8_lossy(&refused.stderr).contains("another operating system"));
    assert!(
        !run(&["set-os", TASK, "--os", other, "--clear"])
            .status
            .success()
    );
    let requests = server.join().unwrap();
    assert_eq!(
        requests.len(),
        6,
        "wrong-OS claim and invalid flags never mutate or retry"
    );
    assert_eq!(requests[1].body["labels"], queued["labels"]);
    assert_eq!(requests[4].body["labels"], released["labels"]);
}

#[test]
fn project_agents_reassignment_and_os_retag_keep_exact_actor_and_platform() {
    use amux::{delegation::Origin, task_platform::TaskOs, task_routing::SessionTarget};
    let actor = agent(Provider::Codex);
    let origin = Origin {
        session_id: actor.session_id,
        instance_id: actor.instance_id,
        provider: actor.provider,
        os: actor.os.clone(),
    };
    let windows = SessionTarget {
        origin: Origin {
            session_id: uuid::Uuid::new_v4(),
            instance_id: uuid::Uuid::new_v4(),
            provider: Provider::Claude,
            os: "windows".into(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let mut linux = windows.clone();
    linux.origin.session_id = uuid::Uuid::new_v4();
    linux.origin.os = "linux".into();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64;
    let directory = json!({"canonical_remote":"github.com/nitecon/amux","targets":[
        {"target":windows,"seen_at":now,"expires_at":now+120_000,"display_name":"Windows peer","hostname":"remote"},
        {"target":linux,"seen_at":now,"expires_at":now+120_000},
        {"target":windows,"seen_at":now-120_001,"expires_at":now-1}
    ]});
    let projects = json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]);
    let task = json!({"id":TASK,"project_ident":"demo","status":"todo","labels":["os:windows"]});
    let assigned = json!({"id":TASK,"project_ident":"demo","status":"todo","labels":["os:windows"],"owner_origin":windows.origin,"routing":{"mode":"session","target":windows}});
    let next_os = if actor.os == "windows" {
        TaskOs::Linux
    } else {
        TaskOs::Windows
    };
    let owned = json!({"id":TASK,"project_ident":"demo","status":"in_progress","labels":["keep",format!("os:{}",actor.os)],"owner_origin":origin});
    let released = json!({"id":TASK,"project_ident":"demo","status":"todo","labels":["keep",format!("os:{}",next_os.as_str())],"owner_origin":null,"owner_agent_id":null,"started_at":null,"done_at":null,"routing":{"mode":"unassigned"}});
    let mut stale_route = assigned.clone();
    stale_route["routing"]["target"]["generation"] = json!(uuid::Uuid::new_v4());
    let mut retained_owner = released.clone();
    retained_owner["owner_origin"] = json!(origin);
    let (url, server) = server(vec![
        reply(200, &projects),
        reply(200, &directory),
        reply(200, &task),
        reply(200, &projects),
        reply(200, &directory),
        reply(200, &assigned),
        reply(200, &task),
        reply(200, &projects),
        reply(200, &directory),
        reply(200, &projects),
        reply(200, &owned),
        reply(200, &released),
        reply(200, &task),
        reply(200, &projects),
        reply(200, &directory),
        reply(200, &stale_route),
        reply(200, &projects),
        reply(200, &owned),
        reply(200, &retained_owner),
    ]);
    let gateway = Gateway::new(&url, "fixture".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let agents = gateway
            .task_agents("demo", &actor.project_directory, Some(TaskOs::Windows))
            .await
            .unwrap();
        assert_eq!(
            agents.targets.len(),
            1,
            "stale and wrong-OS targets are excluded"
        );
        assert_eq!(agents.targets[0].target, windows);
        let id = uuid::Uuid::parse_str(TASK).unwrap();
        assert_eq!(
            gateway
                .reassign_agent_task("demo", id, windows.origin.session_id, &actor)
                .await
                .unwrap(),
            assigned
        );
        assert!(
            gateway
                .reassign_agent_task("demo", id, linux.origin.session_id, &actor)
                .await
                .is_err()
        );
        assert_eq!(
            gateway
                .set_task_os("demo", id, Some(next_os), &actor)
                .await
                .unwrap(),
            released
        );
        assert!(
            gateway
                .reassign_agent_task("demo", id, windows.origin.session_id, &actor)
                .await
                .is_err(),
            "stale exact routing is not an acknowledged handoff"
        );
        assert!(
            gateway
                .set_task_os("demo", id, Some(next_os), &actor)
                .await
                .is_err(),
            "retained incompatible ownership cannot acknowledge OS handoff"
        );
    });
    let requests = server.join().unwrap();
    assert_eq!(
        requests.len(),
        19,
        "wrong OS does not POST and mutations are never retried"
    );
    assert!(
        requests[5]
            .head
            .starts_with(&format!("post /v1/projects/demo/tasks/{TASK}/reassign "))
    );
    assert_eq!(requests[5].body, json!({"target":windows}));
    assert!(
        requests[5]
            .head
            .contains(&format!("x-agent-session-id: {}", actor.session_id))
    );
    assert_eq!(requests[11].body["labels"], released["labels"]);
    assert!(
        requests[11]
            .head
            .contains(&format!("x-agent-instance-id: {}", actor.instance_id))
    );
}

#[test]
fn cli_project_agent_discovery_is_read_only_and_reports_remote_os() {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64;
    let directory = json!({"canonical_remote":"github.com/nitecon/amux","targets":[{"target":{"origin":{"session_id":uuid::Uuid::new_v4(),"instance_id":uuid::Uuid::new_v4(),"provider":"codex","os":"windows"},"generation":uuid::Uuid::new_v4()},"seen_at":now,"expires_at":now+120_000,"display_name":"Remote agent","hostname":"Windows host"}]});
    let (url, server) = server(vec![
        reply(
            200,
            &json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]),
        ),
        reply(200, &directory),
    ]);
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_amux"))
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .env("AMUX_GATEWAY_KEY", "fixture")
        .args([
            "tasks",
            "--project",
            "demo",
            "--gateway-url",
            &url,
            "--json",
            "agents",
            "--os",
            "windows",
        ])
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        serde_json::from_slice::<Value>(&output.stdout).unwrap(),
        directory
    );
    for request in server.join().unwrap() {
        assert!(request.head.starts_with("get "));
        assert!(!request.head.contains("x-agent-"));
    }
}

#[test]
fn user_review_requires_exact_owner_and_cleared_ack_then_only_a_human_requeues() {
    let actor = agent(Provider::Codex);
    let origin = json!({"session_id":actor.session_id,"instance_id":actor.instance_id,"provider":actor.provider,"os":actor.os});
    let owned =
        json!({"id":TASK,"project_ident":"demo","status":"in_progress","owner_origin":origin});
    let parked = json!({"id":TASK,"project_ident":"demo","status":"user_review","owner_origin":null,"owner_agent_id":null,"started_at":null,"done_at":null});
    let mut returned = parked.clone();
    returned["status"] = json!("todo");
    let mut wrong = owned.clone();
    wrong["owner_origin"]["instance_id"] = json!(uuid::Uuid::new_v4());
    let mut malformed = parked.clone();
    malformed.as_object_mut().unwrap().remove("owner_origin");
    let (url, server) = server(vec![
        reply(200, &wrong),
        reply(200, &owned),
        reply(200, &parked),
        reply(200, &parked),
        reply(200, &returned),
        reply(200, &owned),
        reply(200, &malformed),
    ]);
    let gateway = Gateway::new(&url, "fixture".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let id = uuid::Uuid::parse_str(TASK).unwrap();
        assert!(
            gateway
                .request_user_review("demo", id, &actor)
                .await
                .is_err()
        );
        assert_eq!(
            gateway
                .request_user_review("demo", id, &actor)
                .await
                .unwrap(),
            parked
        );
        assert_eq!(
            gateway.resume_desktop_task("demo", id).await.unwrap(),
            returned
        );
        assert!(
            gateway
                .request_user_review("demo", id, &actor)
                .await
                .is_err(),
            "missing ownership fields cannot acknowledge release"
        );
    });
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 7, "no blind retries");
    assert_eq!(requests[2].body, json!({"status":"user_review"}));
    assert!(
        requests[2]
            .head
            .contains(&format!("x-agent-instance-id: {}", actor.instance_id))
    );
    assert_eq!(requests[4].body, json!({"status":"todo"}));
    assert!(
        !requests[4].head.contains("x-agent-"),
        "user returns work without impersonating an agent"
    );
}

#[test]
fn human_requeue_of_a_finding_requires_confirmed_removal_of_the_hold() {
    let parked = json!({"id":TASK,"project_ident":"demo","status":"user_review",
        "labels":["autoreview-finding","os:linux","bug"],
        "owner_origin":null,"owner_agent_id":null,"started_at":null,"done_at":null});
    let mut returned = parked.clone();
    returned["status"] = json!("todo");
    returned["labels"] = json!(["os:linux", "bug"]);
    let mut held = returned.clone();
    held["labels"] = parked["labels"].clone();
    let mut missing_labels = returned.clone();
    missing_labels.as_object_mut().unwrap().remove("labels");
    let mut wrong_id = returned.clone();
    wrong_id["id"] = json!(uuid::Uuid::new_v4());
    let replies = [
        returned.clone(),
        held,
        missing_labels,
        parked.clone(),
        wrong_id,
    ];
    let (url, server) = server(
        replies
            .iter()
            .flat_map(|result| [reply(200, &parked), reply(200, result)])
            .collect(),
    );
    let gateway = Gateway::new(&url, "fixture".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let id = uuid::Uuid::parse_str(TASK).unwrap();
        assert_eq!(
            gateway.resume_desktop_task("demo", id).await.unwrap(),
            returned
        );
        for _ in 1..replies.len() {
            assert!(gateway.resume_desktop_task("demo", id).await.is_err());
        }
    });
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 10, "unconfirmed returns are never retried");
    for request in requests.iter().skip(1).step_by(2) {
        assert_eq!(request.body, json!({"status":"todo"}));
        assert!(
            !request.head.contains("x-agent-"),
            "only the human can approve"
        );
    }
}

#[test]
fn human_requeue_of_an_ordinary_task_must_clear_its_exact_session_route() {
    let actor = agent(Provider::Codex);
    let parked = json!({"id":TASK,"project_ident":"demo","status":"user_review",
        "labels":[], "owner_origin":null,"owner_agent_id":null,"started_at":null,"done_at":null,
        "routing":{"mode":"session","target":{"origin":{
            "session_id":actor.session_id,"instance_id":actor.instance_id,
            "provider":actor.provider,"os":actor.os},"generation":uuid::Uuid::new_v4()}}});
    let mut pinned = parked.clone();
    pinned["status"] = json!("todo");
    let mut returned = pinned.clone();
    returned["routing"] = json!({"mode":"auto"});
    let mut missing_route = returned.clone();
    missing_route.as_object_mut().unwrap().remove("routing");
    let (url, server) = server(vec![
        reply(200, &parked),
        reply(200, &pinned),
        reply(200, &parked),
        reply(200, &missing_route),
        reply(200, &parked),
        reply(200, &returned),
    ]);
    let gateway = Gateway::new(&url, "fixture".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let id = uuid::Uuid::parse_str(TASK).unwrap();
        assert!(gateway.resume_desktop_task("demo", id).await.is_err());
        assert!(gateway.resume_desktop_task("demo", id).await.is_err());
        assert_eq!(
            gateway.resume_desktop_task("demo", id).await.unwrap(),
            returned
        );
    });
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 6);
    for request in requests.iter().skip(1).step_by(2) {
        assert_eq!(request.body, json!({"status":"todo"}));
        assert!(!request.head.contains("x-agent-"));
    }
}

#[test]
fn cli_can_park_owned_work_for_user_review_without_provider_environment() {
    let base = Path::new(env!("CARGO_TARGET_TMPDIR")).join("review-gateway");
    std::fs::create_dir_all(&base).unwrap();
    let state = tempfile::tempdir_in(base).unwrap();
    let actor = agent(Provider::Codex);
    let record = amux::identity::AgentRecord {
        identity: actor.clone(),
        pid: 0,
        process_start: 0,
        process_generation: "unavailable".into(),
    };
    std::fs::create_dir(state.path().join("agents")).unwrap();
    std::fs::write(
        state
            .path()
            .join("agents")
            .join(format!("{}.json", actor.session_id)),
        serde_json::to_vec(&record).unwrap(),
    )
    .unwrap();
    let owned = json!({"id":TASK,"project_ident":"demo","status":"in_progress","owner_origin":{"session_id":actor.session_id,"instance_id":actor.instance_id,"provider":actor.provider,"os":actor.os}});
    let parked = json!({"id":TASK,"project_ident":"demo","status":"user_review","owner_agent_id":null,"owner_origin":null,"started_at":null,"done_at":null});
    let (url, server) = server(vec![reply(200, &owned), reply(200, &parked)]);
    let mut command = std::process::Command::new(env!("CARGO_BIN_EXE_amux"));
    for name in actor.environment().keys() {
        command.env_remove(name);
    }
    let output = command
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .env("AMUX_STATE_DIR", state.path())
        .env("AMUX_GATEWAY_KEY", "fixture")
        .args([
            "--agent",
            &actor.session_id.to_string(),
            "tasks",
            "--project",
            "demo",
            "--gateway-url",
            &url,
            "--json",
            "user-review",
            TASK,
        ])
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        serde_json::from_slice::<Value>(&output.stdout).unwrap()["status"],
        "user_review"
    );
    assert_eq!(server.join().unwrap().len(), 2);
}

#[test]
fn desktop_backlog_accepts_project_scoped_summary_rows_without_detail_only_fields() {
    use amux::delegation::{Origin, Target};
    let actor = agent(Provider::Codex);
    let target = Target {
        workspace: uuid::Uuid::new_v4(),
        terminal: 1,
        session: actor.session_id,
        agent_id: Some(actor.gateway_id()),
        pid: 5,
        generation: "fixture".into(),
        directory: actor.project_directory.clone(),
        project: "demo".into(),
        origin: Some(Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        }),
        goal: None,
    };
    // The live gateway list contains summaries, not GET /tasks/{id} details:
    // project_ident and owner_origin are absent, even for visible queued tasks.
    let summary = json!({"id":TASK, "title":"Queued gateway work", "status":"todo",
        "owner_agent_id":null, "goal":null, "kind":"normal", "rank":1,
        "updated_at":10, "routing":{"mode":"auto"}, "comment_count":1});
    let mut foreign = summary.clone();
    foreign["project_ident"] = json!("other-repository");
    let mut detail = summary.clone();
    detail["project_ident"] = json!("demo");
    let mut claimed = detail.clone();
    claimed["status"] = json!("in_progress");
    claimed["owner_origin"] = json!(target.origin);
    let projects = json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]);
    let (url, server) = server(vec![
        reply(200, &json!([summary])),
        reply(200, &projects),
        reply(200, &detail),
        reply(200, &claimed),
        reply(200, &json!({"id":uuid::Uuid::new_v4(),"task_id":TASK})),
        reply(200, &json!([foreign])),
    ]);
    let gateway = Gateway::new(&url, "fixture".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let tasks = gateway.list_all("demo").await.unwrap();
        assert!(
            amux::backlog::eligible(&tasks[0], &target),
            "visible unassigned summary must enter dispatch"
        );
        assert_eq!(tasks[0]["project_ident"], "demo");
        assert!(
            gateway
                .claim_backlog(uuid::Uuid::parse_str(TASK).unwrap(), 10, &target, &actor)
                .await
                .unwrap()
                .is_some(),
            "summary must proceed through fresh detail, exact claim and viability comment"
        );
        assert!(
            gateway.list_all("demo").await.is_err(),
            "never replace an explicit foreign project"
        );
    });
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 6);
    assert!(
        requests[0]
            .head
            .starts_with("get /v1/projects/demo/tasks?status=")
            && requests[5]
                .head
                .starts_with("get /v1/projects/demo/tasks?status=")
    );
    assert!(
        requests[2]
            .head
            .starts_with(&format!("get /v1/projects/demo/tasks/{TASK} "))
    );
    assert!(requests[3].head.starts_with("patch "));
    assert_eq!(requests[4].body["content"], amux::backlog::REVIEW_COMMENT);
}

#[test]
fn backlog_claim_rechecks_repository_revision_and_owner_before_one_agent_mutation() {
    use amux::delegation::{Origin, Target};
    let actor = agent(Provider::Codex);
    let target = Target {
        workspace: uuid::Uuid::new_v4(),
        terminal: 1,
        session: actor.session_id,
        agent_id: Some(actor.gateway_id()),
        pid: 5,
        generation: "fixture".into(),
        directory: actor.project_directory.clone(),
        project: "demo".into(),
        origin: Some(Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        }),
        goal: None,
    };
    let projects = json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]);
    let queued = json!({"id":TASK,"project_ident":"demo","status":"todo","updated_at":10,"routing":{"mode":"unassigned"}});
    let mut claimed = owned_by(&actor);
    claimed["owner_origin"] = json!(target.origin);
    let mut other = queued.clone();
    other["owner_agent_id"] = json!("Other agent");
    let mut changed = queued.clone();
    changed["updated_at"] = json!(11);
    let comment =
        json!({"id":uuid::Uuid::new_v4(), "task_id":TASK, "content":amux::backlog::REVIEW_COMMENT});
    let (url, server) = server(vec![
        reply(200, &projects),
        reply(200, &queued),
        reply(200, &claimed),
        reply(200, &comment),
        reply(200, &projects),
        reply(200, &other),
        reply(200, &projects),
        reply(200, &changed),
        reply(200, &projects),
        reply(200, &queued),
        reply(409, &json!({"error":"already claimed"})),
        reply(200, &projects),
        reply(200, &queued),
        reply(200, &claimed),
        reply(503, &json!({"error":"comment acknowledgment unavailable"})),
    ]);
    let gateway = Gateway::new(&url, "fixture".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let task = uuid::Uuid::parse_str(TASK).unwrap();
        assert!(
            gateway
                .claim_backlog(task, 10, &target, &actor)
                .await
                .unwrap()
                .is_some()
        );
        assert!(
            gateway
                .claim_backlog(task, 10, &target, &actor)
                .await
                .unwrap()
                .is_none()
        );
        assert!(
            gateway
                .claim_backlog(task, 10, &target, &actor)
                .await
                .unwrap()
                .is_none()
        );
        assert!(
            gateway
                .claim_backlog(task, 10, &target, &actor)
                .await
                .is_err()
        );
        assert!(
            gateway
                .claim_backlog(task, 10, &target, &actor)
                .await
                .is_err(),
            "an uncertain comment must not acknowledge a completed handoff"
        );
    });
    let requests = server.join().unwrap();
    let comments = requests
        .iter()
        .filter(|r| r.head.starts_with("post ") && r.head.contains("/comments "))
        .collect::<Vec<_>>();
    assert_eq!(
        comments.len(),
        2,
        "only confirmed claims get comments; no retry after a lost acknowledgment"
    );
    assert_eq!(comments[0].body["content"], amux::backlog::REVIEW_COMMENT);
    assert!(
        comments[0]
            .head
            .contains(&format!("x-agent-session-id: {}", actor.session_id))
    );
    let mutations = requests
        .iter()
        .filter(|r| r.head.starts_with("patch "))
        .collect::<Vec<_>>();
    assert_eq!(
        mutations.len(),
        3,
        "one attempt per request, no retries or claims after stale reads"
    );
    for request in mutations {
        assert!(
            request
                .head
                .contains(&format!("x-agent-session-id: {}", actor.session_id))
        );
        assert_eq!(request.body["status"], "in_progress");
    }
}

#[test]
fn sidebar_actions_are_human_requests_and_confirm_the_requested_result() {
    use amux::{delegation::Origin, task_management::TaskEdits, task_routing::SessionTarget};
    let target = SessionTarget {
        origin: Origin {
            session_id: uuid::Uuid::new_v4(),
            instance_id: uuid::Uuid::new_v4(),
            provider: Provider::Codex,
            os: "windows".into(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let task = json!({"id":TASK,"project_ident":"demo","status":"todo", "title":"Updated title","description":"Two\nlines", "specification":"Exact handoff\nwith newlines"});
    let (url, server) = server(vec![
        reply(200, &task),
        reply(
            200,
            &json!({"id":uuid::Uuid::new_v4(),"task_id":TASK,"content":"User comment\nsecond line","author_type":"user"}),
        ),
        reply(
            200,
            &json!({"id":TASK,"project_ident":"demo","status":"todo","owner_origin":target.origin}),
        ),
        reply(
            200,
            &json!({"id":TASK,"project_ident":"demo","status":"done"}),
        ),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let id = uuid::Uuid::parse_str(TASK).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        gateway
            .edit_desktop_task(
                "demo",
                id,
                &TaskEdits {
                    title: "Updated title".into(),
                    description: "Two\nlines".into(),
                    specification: "Exact handoff\nwith newlines".into(),
                },
            )
            .await
            .unwrap();
        gateway
            .comment_on_task("demo", id, "User comment\nsecond line")
            .await
            .unwrap();
        gateway
            .assign_desktop_task("demo", id, &target)
            .await
            .unwrap();
        gateway.close_desktop_task("demo", id).await.unwrap();
    });
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 4);
    for request in &requests {
        assert!(request.head.contains("authorization: bearer test-only"));
        assert!(
            !request.head.contains("x-agent-"),
            "desktop actions must not impersonate the assigned agent"
        );
    }
    assert!(
        requests[0]
            .head
            .starts_with(&format!("patch /v1/projects/demo/tasks/{TASK} "))
    );
    assert_eq!(requests[0].body["description"], "Two\nlines");
    assert_eq!(
        requests[0].body["specification"],
        "Exact handoff\nwith newlines"
    );
    assert!(
        requests[1]
            .head
            .starts_with(&format!("post /v1/projects/demo/tasks/{TASK}/comments "))
    );
    assert_eq!(requests[1].body["author_type"], "user");
    assert!(
        requests[2]
            .head
            .starts_with(&format!("post /v1/projects/demo/tasks/{TASK}/assign "))
    );
    assert_eq!(requests[2].body, json!({"target":target}));
    assert_eq!(requests[3].body, json!({"status":"done"}));
}

#[test]
fn sidebar_rejects_wrong_owners_and_failed_posts_without_replay() {
    use amux::{delegation::Origin, task_routing::SessionTarget};
    let target = SessionTarget {
        origin: Origin {
            session_id: uuid::Uuid::new_v4(),
            instance_id: uuid::Uuid::new_v4(),
            provider: Provider::Codex,
            os: "linux".into(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let (url, server) = server(vec![
        reply(
            200,
            &json!({"id":TASK,"project_ident":"demo","status":"todo","owner_origin":null}),
        ),
        reply(409, &json!({"error":"stale target"})),
        reply(
            200,
            &json!({"id":uuid::Uuid::new_v4(),"task_id":uuid::Uuid::new_v4(),"content":"comment","author_type":"user"}),
        ),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let id = uuid::Uuid::parse_str(TASK).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        assert!(
            gateway
                .assign_desktop_task("demo", id, &target)
                .await
                .is_err()
        );
        assert!(
            gateway
                .assign_desktop_task("demo", id, &target)
                .await
                .is_err()
        );
        assert!(
            gateway
                .comment_on_task("demo", id, "comment")
                .await
                .is_err()
        );
    });
    assert_eq!(
        server.join().unwrap().len(),
        3,
        "never replay a user mutation after an unconfirmed response"
    );
}

#[test]
fn presence_labels_fall_back_only_for_old_gateway_schema() {
    use amux::task_routing::SessionTarget;
    let actor = agent(Provider::Codex);
    let target = SessionTarget {
        origin: amux::delegation::Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let available = json!({"target":target,"seen_at":1000000,"expires_at":1120000,"canonical_remote":"github.com/nitecon/amux"});
    let (url, server) = server(vec![
        reply(400, &json!({"error":"unknown field display_name"})),
        reply(200, &available),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    tokio::runtime::Runtime::new()
        .unwrap()
        .block_on(gateway.advertise_task_target(
            "demo",
            Some("github.com/nitecon/amux"),
            &actor,
            target.generation,
        ))
        .unwrap();
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].body["display_name"], actor.display_name());
    assert_eq!(
        requests[1].body,
        json!({"generation":target.generation,"canonical_remote":"github.com/nitecon/amux"})
    );
}

#[test]
fn desktop_presence_and_selected_task_use_bearer_without_impersonating_the_recipient() {
    use amux::{
        delegation::Origin,
        task_routing::{Routing, SessionTarget},
    };
    let actor = agent(Provider::Codex);
    let target = SessionTarget {
        origin: Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let routing = Routing::Session {
        target: target.clone(),
    };
    let available = json!({"target":target,"seen_at":1000000,"expires_at":1120000,"canonical_remote":"github.com/nitecon/amux"});
    let accepted = json!({"id": TASK,"project_ident":"demo","status":"todo","routing":routing,"owner_origin":target.origin});
    let (url, server) = server(vec![
        reply(200, &available),
        reply(
            200,
            &json!({"targets":[available],"canonical_remote":"github.com/nitecon/amux"}),
        ),
        reply(
            200,
            &json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]),
        ),
        reply(200, &accepted),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    runtime.block_on(async {
        gateway
            .advertise_task_target(
                "demo",
                Some("github.com/nitecon/amux"),
                &actor,
                target.generation,
            )
            .await
            .unwrap();
        let directory = gateway
            .task_targets("demo", Some("github.com/nitecon/amux"))
            .await
            .unwrap();
        assert_eq!(directory.targets[0].target, target);
        assert_eq!(
            gateway
                .create_routed_task(
                    "demo",
                    "Human task",
                    "exact\ncontext",
                    Some("github.com/nitecon/amux"),
                    &routing
                )
                .await
                .unwrap()
                .to_string(),
            TASK
        );
    });
    let requests = server.join().unwrap();
    for request in &requests {
        assert!(request.head.contains("authorization: bearer test-only"));
        assert!(!request.head.contains("x-machine"));
    }
    assert!(
        requests[0]
            .head
            .starts_with("post /v1/projects/demo/task-targets ")
    );
    assert!(
        requests[0]
            .head
            .contains(&format!("x-agent-session-id: {}", actor.session_id))
    );
    assert_eq!(
        requests[0].body,
        json!({"generation":target.generation,"canonical_remote":"github.com/nitecon/amux", "display_name":actor.display_name(),"hostname":sysinfo::System::host_name()})
    );
    assert!(
        requests[1]
            .head
            .starts_with("get /v1/projects/demo/task-targets ")
    );
    assert!(!requests[1].head.contains("x-agent-"));
    assert!(!requests[2].head.contains("x-agent-"));
    assert!(!requests[3].head.contains("x-agent-"));
    assert_eq!(
        requests[3].body["routing"],
        serde_json::to_value(routing).unwrap()
    );
    assert_eq!(requests[3].body["specification"], "exact\ncontext");
}

#[test]
fn routed_task_errors_never_retry_or_silently_fall_back_to_auto() {
    use amux::task_routing::Routing;
    let (url, server) = server(vec![
        reply(
            200,
            &json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]),
        ),
        reply(409, &json!({"error":"stale target"})),
        reply(
            200,
            &json!([{"ident":"demo","canonical_remote":"github.com/nitecon/amux"}]),
        ),
        reply(
            200,
            &json!({"id":TASK,"project_ident":"demo","status":"todo","routing":{"mode":"auto"}}),
        ),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    assert!(
        runtime
            .block_on(gateway.create_routed_task(
                "demo",
                "Queued",
                "",
                Some("github.com/nitecon/amux"),
                &Routing::Unassigned
            ))
            .is_err()
    );
    assert!(
        runtime
            .block_on(gateway.create_routed_task(
                "demo",
                "Broadcast",
                "",
                Some("github.com/nitecon/amux"),
                &Routing::Broadcast
            ))
            .is_err()
    );
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 4);
    assert_eq!(requests[1].body["routing"]["mode"], "unassigned");
    assert_eq!(requests[3].body["routing"]["mode"], "broadcast");
    assert!(requests.iter().all(|r| !r.head.contains("x-agent-")));
}

#[test]
fn recipient_directory_from_another_repository_is_rejected() {
    let (url, server) = server(vec![reply(
        200,
        &json!({"targets":[],"canonical_remote":"github.com/nitecon/other"}),
    )]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    assert!(
        tokio::runtime::Runtime::new()
            .unwrap()
            .block_on(gateway.task_targets("demo", Some("github.com/nitecon/amux")))
            .is_err()
    );
    assert_eq!(server.join().unwrap().len(), 1);
}

#[test]
fn local_only_project_routes_with_a_null_remote_and_never_matches_a_repository() {
    use amux::{
        delegation::Origin,
        task_routing::{Routing, SessionTarget},
    };
    let actor = agent(Provider::Codex);
    let target = SessionTarget {
        origin: Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        },
        generation: uuid::Uuid::new_v4(),
    };
    let routing = Routing::Session {
        target: target.clone(),
    };
    let available =
        json!({"target":target,"seen_at":1000000,"expires_at":1120000,"canonical_remote":null});
    let accepted = json!({"id": TASK,"project_ident":"games","status":"todo","routing":routing,"owner_origin":target.origin});
    let adhoc =
        json!([{"ident":"games","kind":"adhoc","canonical_remote":null,"archived_at":null}]);
    let (url, server) = server(vec![
        reply(200, &available),
        reply(200, &json!({"targets":[available],"canonical_remote":null})),
        reply(200, &adhoc),
        reply(200, &accepted),
        // A repository-backed directory or acknowledgement never satisfies a local root.
        reply(
            200,
            &json!({"targets":[],"canonical_remote":"github.com/nitecon/games"}),
        ),
        reply(
            200,
            &json!({"target":target,"seen_at":1000000,"expires_at":1120000,"canonical_remote":"github.com/nitecon/games"}),
        ),
        // Explicit routing without a remote requires an ad-hoc registry entry.
        reply(
            200,
            &json!([{"ident":"games","kind":"repo","canonical_remote":"github.com/nitecon/games","archived_at":null}]),
        ),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        gateway
            .advertise_task_target("games", None, &actor, target.generation)
            .await
            .unwrap();
        let directory = gateway.task_targets("games", None).await.unwrap();
        assert_eq!(directory.canonical_remote, None);
        gateway
            .create_routed_task("games", "Local", "context", None, &routing)
            .await
            .unwrap();
        assert!(gateway.task_targets("games", None).await.is_err());
        assert!(
            gateway
                .advertise_task_target("games", None, &actor, target.generation)
                .await
                .is_err()
        );
        assert!(
            gateway
                .create_routed_task("games", "Local", "context", None, &routing)
                .await
                .is_err()
        );
    });
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 7);
    assert!(requests[0].body["canonical_remote"].is_null());
    assert!(requests[3].body.get("canonical_remote").is_none());
    assert_eq!(requests[3].body["routing"]["mode"], "session");
}

#[test]
fn changed_repository_project_prevents_even_an_automatic_task_post() {
    let (url, server) = server(vec![reply(
        200,
        &json!([{"ident":"other","canonical_remote":"github.com/nitecon/amux"}]),
    )]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    assert!(
        tokio::runtime::Runtime::new()
            .unwrap()
            .block_on(gateway.create_routed_task(
                "demo",
                "Do not misroute",
                "",
                Some("github.com/nitecon/amux"),
                &amux::task_routing::Routing::Auto
            ))
            .is_err()
    );
    let seen = server.join().unwrap();
    assert_eq!(seen.len(), 1);
    assert!(seen[0].head.starts_with("get /v1/projects "));
}

#[test]
fn three_same_project_agents_each_claim_with_only_their_own_identity() {
    let agents = [
        agent(Provider::Codex),
        agent(Provider::Codex),
        agent(Provider::Claude),
    ];
    let (url, server) = server(agents.iter().map(|a| reply(200, &owned_by(a))).collect());
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for actor in &agents {
        runtime
            .block_on(gateway.claim("demo", TASK, actor))
            .unwrap();
    }
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 3);
    for (actor, request) in agents.iter().zip(&requests) {
        let head = &request.head;
        assert!(head.starts_with(&format!("patch /v1/projects/demo/tasks/{TASK} ")));
        assert!(head.contains("authorization: bearer test-only"));
        assert!(head.contains(&format!(
            "x-agent-id: {}",
            actor.display_name().to_lowercase()
        )));
        assert!(head.contains(&format!("x-agent-session-id: {}", actor.session_id)));
        assert!(head.contains(&format!("x-agent-instance-id: {}", actor.instance_id)));
        assert!(head.contains(&format!("x-agent-provider: {}", actor.provider.command())));
        assert!(head.contains(&format!("x-agent-os: {}", actor.os)));
        assert_eq!(request.body["status"], "in_progress");
        assert_eq!(request.body["owner_agent_id"], actor.display_name());
        // No request may carry a peer's identity.
        for peer in agents.iter().filter(|p| *p != actor) {
            assert!(!head.contains(&peer.session_id.to_string()));
            assert!(!head.contains(&peer.instance_id.to_string()));
        }
    }
}

#[test]
fn assigned_cli_claim_works_when_provider_environment_and_ancestry_are_unavailable() {
    let base = Path::new(env!("CARGO_TARGET_TMPDIR")).join("assigned-gateway");
    std::fs::create_dir_all(&base).unwrap();
    let state = tempfile::tempdir_in(base).unwrap();
    let actor = agent(Provider::Codex);
    let record = amux::identity::AgentRecord {
        identity: actor.clone(),
        pid: 0,
        process_start: 0,
        process_generation: "unavailable".into(),
    };
    std::fs::create_dir(state.path().join("agents")).unwrap();
    std::fs::write(
        state
            .path()
            .join("agents")
            .join(format!("{}.json", actor.session_id)),
        serde_json::to_vec(&record).unwrap(),
    )
    .unwrap();
    let (url, server) = server(vec![
        reply(200, &owned_by(&actor)),
        reply(200, &owned_by(&actor)),
    ]);
    let mut command = std::process::Command::new(env!("CARGO_BIN_EXE_amux"));
    for name in actor.environment().keys() {
        command.env_remove(name);
    }
    let output = command
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .env("AMUX_STATE_DIR", state.path())
        .env("AMUX_GATEWAY_KEY", "test-only")
        .args([
            "--agent",
            &actor.session_id.to_string(),
            "tasks",
            "--project",
            "demo",
            "--gateway-url",
            &url,
            "claim",
            TASK,
        ])
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = server.join().unwrap();
    assert!(requests[0].head.starts_with("get "));
    assert_eq!(requests[1].body["owner_agent_id"], actor.display_name());
    assert!(
        requests[1]
            .head
            .contains(&format!("x-agent-session-id: {}", actor.session_id))
    );
}

#[test]
fn claim_succeeds_only_when_the_gateway_acknowledges_this_exact_execution() {
    let actor = agent(Provider::Claude);
    let peer = agent(Provider::Claude);
    let mut other_instance = owned_by(&actor);
    other_instance["owner_origin"]["instance_id"] = json!(peer.instance_id.to_string());
    let mut not_claimed = owned_by(&actor);
    not_claimed["status"] = json!("todo");
    let mut legacy = owned_by(&actor);
    legacy.as_object_mut().unwrap().remove("owner_origin");
    let refusals = [
        reply(200, &owned_by(&peer)),
        reply(200, &other_instance),
        reply(200, &not_claimed),
        reply(200, &legacy),
        reply(409, &json!({"error": "already claimed"})),
        b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec(),
    ];
    let (url, server) = server(refusals.to_vec());
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for _ in 0..refusals.len() {
        assert!(
            runtime
                .block_on(gateway.claim("demo", TASK, &actor))
                .is_err(),
            "a claim must not be reported without exact owner acknowledgment"
        );
    }
    // A malformed task ID is rejected before any request is sent.
    assert!(
        runtime
            .block_on(gateway.claim("demo", "01a164c4", &actor))
            .is_err()
    );
    assert!(
        runtime
            .block_on(gateway.claim("demo", &uuid::Uuid::nil().to_string(), &actor))
            .is_err()
    );
    assert_eq!(server.join().unwrap().len(), refusals.len(), "no retries");
}

#[test]
fn task_creation_requires_a_canonical_task_in_the_requested_project() {
    let actor = agent(Provider::Codex);
    let accepted = json!({"id":TASK,"project_ident":"demo","status":"todo"});
    let mut wrong_project = accepted.clone();
    wrong_project["project_ident"] = json!("peer-project");
    let mut nil_id = accepted.clone();
    nil_id["id"] = json!(uuid::Uuid::nil().to_string());
    let mut invalid_id = accepted.clone();
    invalid_id["id"] = json!("not-a-task-UUID");
    let mut missing_id = accepted.clone();
    missing_id.as_object_mut().unwrap().remove("id");
    let mut missing_project = accepted.clone();
    missing_project
        .as_object_mut()
        .unwrap()
        .remove("project_ident");
    let mut unknown_status = accepted.clone();
    unknown_status["status"] = json!("accepted-somewhere");
    let refusals = [
        b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\nConnection: close\r\n\r\n".to_vec(),
        reply(200, &Value::Null),
        reply(200, &wrong_project),
        reply(200, &nil_id),
        reply(200, &invalid_id),
        reply(200, &missing_id),
        reply(200, &missing_project),
        reply(200, &unknown_status),
    ];
    let mut replies = refusals.to_vec();
    replies.push(reply(201, &accepted));
    let (url, server) = server(replies);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let body = json!({"title":"handoff title","specification":"literal\nreview context"});
    for _ in 0..refusals.len() {
        let result = runtime.block_on(gateway.request(
            Method::POST,
            "demo",
            &[],
            Some(body.clone()),
            Some(&actor),
        ));
        assert!(
            result.is_err(),
            "creation acceptance is unproven: {result:?}"
        );
    }
    assert_eq!(
        runtime
            .block_on(gateway.request(Method::POST, "demo", &[], Some(body.clone()), Some(&actor),))
            .unwrap(),
        accepted
    );
    let requests = server.join().unwrap();
    assert_eq!(
        requests.len(),
        refusals.len() + 1,
        "never retry uncertain creation"
    );
    assert!(requests.iter().all(|request| request.body == body));
}

#[test]
fn a_matching_owner_cannot_acknowledge_a_different_task_or_project() {
    let actor = agent(Provider::Codex);
    let mut wrong_task = owned_by(&actor);
    wrong_task["id"] = json!(uuid::Uuid::new_v4().to_string());
    let mut missing_task = owned_by(&actor);
    missing_task.as_object_mut().unwrap().remove("id");
    let mut wrong_project = owned_by(&actor);
    wrong_project["project_ident"] = json!("peer-project");
    let mut missing_project = owned_by(&actor);
    missing_project
        .as_object_mut()
        .unwrap()
        .remove("project_ident");
    let replies: Vec<_> = [wrong_task, missing_task, wrong_project, missing_project]
        .iter()
        .map(|body| reply(200, body))
        .collect();
    let count = replies.len();
    let (url, server) = server(replies);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for _ in 0..count {
        let result = runtime.block_on(gateway.claim("demo", TASK, &actor));
        assert!(
            result.is_err(),
            "owner alone does not identify the task: {result:?}"
        );
    }
    assert_eq!(server.join().unwrap().len(), count, "no blind claim retry");
}

#[test]
fn task_reads_updates_and_comments_require_the_requested_resource() {
    let actor = agent(Provider::Codex);
    let task = json!({"id":TASK,"project_ident":"demo","status":"done"});
    let mut wrong_task = task.clone();
    wrong_task["id"] = json!(uuid::Uuid::new_v4().to_string());
    let mut wrong_project = task.clone();
    wrong_project["project_ident"] = json!("peer-project");
    let mut wrong_status = task.clone();
    wrong_status["status"] = json!("in_progress");
    let comment = json!({"id":uuid::Uuid::new_v4().to_string(),"task_id":TASK,"content":"context"});
    let mut wrong_parent = comment.clone();
    wrong_parent["task_id"] = json!(uuid::Uuid::new_v4().to_string());
    let mut missing_comment = comment.clone();
    missing_comment.as_object_mut().unwrap().remove("id");
    let replies = vec![
        reply(200, &wrong_task),
        reply(200, &wrong_project),
        reply(200, &task),
        reply(200, &wrong_task),
        reply(200, &wrong_status),
        reply(200, &task),
        reply(200, &wrong_parent),
        reply(200, &missing_comment),
        reply(200, &comment),
    ];
    let (url, server) = server(replies);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for method in [Method::GET, Method::PATCH, Method::POST] {
        let (suffix, body) = if method == Method::POST {
            (vec![TASK, "comments"], Some(json!({"content":"context"})))
        } else if method == Method::PATCH {
            (vec![TASK], Some(json!({"status":"done"})))
        } else {
            (vec![TASK], None)
        };
        for _ in 0..2 {
            let result = runtime.block_on(gateway.request(
                method.clone(),
                "demo",
                &suffix,
                body.clone(),
                Some(&actor),
            ));
            assert!(
                result.is_err(),
                "reply did not identify the requested resource/state: {result:?}"
            );
        }
        assert_eq!(
            runtime
                .block_on(gateway.request(method.clone(), "demo", &suffix, body, Some(&actor)))
                .unwrap(),
            if method == Method::POST {
                comment.clone()
            } else {
                task.clone()
            }
        );
    }
    assert_eq!(
        server.join().unwrap().len(),
        9,
        "no retry on ambiguous responses"
    );
}

#[test]
fn list_filters_and_rank_use_the_agent_tools_wire_contract() {
    let actor = agent(Provider::Codex);
    let ranked = json!({"id":TASK,"project_ident":"demo","status":"todo","rank":3});
    let mut unranked = ranked.clone();
    unranked["rank"] = json!(9);
    let (url, server) = server(vec![
        reply(200, &json!([])),
        reply(200, &json!([])),
        reply(200, &unranked),
        reply(200, &ranked),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    runtime
        .block_on(gateway.list_filtered("demo", None, false))
        .unwrap();
    runtime
        .block_on(gateway.list_filtered("demo", Some("done, cancelled"), true))
        .unwrap();
    assert!(
        runtime
            .block_on(gateway.list_filtered("demo", Some("done,closed"), false))
            .is_err(),
        "unknown statuses are refused before any request"
    );
    let task = uuid::Uuid::parse_str(TASK).unwrap();
    assert!(
        runtime
            .block_on(gateway.rank("demo", task, 3, &actor))
            .is_err(),
        "an unacknowledged rank is unconfirmed"
    );
    assert_eq!(
        runtime
            .block_on(gateway.rank("demo", task, 3, &actor))
            .unwrap(),
        ranked
    );
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 4, "no retry after an unconfirmed rank");
    assert!(
        requests[0]
            .head
            .starts_with("get /v1/projects/demo/tasks http/1.1")
    );
    assert!(requests[1].head.starts_with(
        "get /v1/projects/demo/tasks?status=done%2ccancelled&include_stale=true http/1.1"
    ));
    for request in &requests[2..] {
        assert!(
            request
                .head
                .starts_with(&format!("patch /v1/projects/demo/tasks/{TASK} http/1.1"))
        );
        assert_eq!(request.body, json!({"rank":3}));
        assert!(
            request
                .head
                .contains(&format!("x-agent-session-id: {}", actor.session_id))
        );
    }
}

#[test]
fn task_details_merge_metadata_but_never_routing_labels() {
    let details = TaskDetails {
        description: Some("Why this matters.".into()),
        labels: vec!["enhancement".into()],
        hostname: Some("Defcon2".into()),
    };
    let mut body = json!({"title":"Port","labels":["os:windows"]});
    details.apply(&mut body).unwrap();
    assert_eq!(
        body,
        json!({"title":"Port","labels":["os:windows","enhancement"],"description":"Why this matters.","hostname":"Defcon2"})
    );
    let mut untouched = json!({"title":"Port"});
    TaskDetails::default().apply(&mut untouched).unwrap();
    assert_eq!(untouched, json!({"title":"Port"}));
    for details in [
        TaskDetails {
            labels: vec!["os:linux".into()],
            ..Default::default()
        },
        TaskDetails {
            labels: vec!["autoreview-finding".into()],
            ..Default::default()
        },
        TaskDetails {
            labels: vec![" ".into()],
            ..Default::default()
        },
        TaskDetails {
            description: Some("x".repeat(64 * 1024 + 1)),
            ..Default::default()
        },
        TaskDetails {
            hostname: Some("host\nname".into()),
            ..Default::default()
        },
    ] {
        assert!(details.apply(&mut json!({})).is_err());
    }
}

#[test]
fn oversized_and_malformed_responses_are_refused() {
    let declared = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        LIMIT + 1
    )
    .into_bytes();
    // Without a declared length the client must still stop at the bound.
    let mut streamed =
        b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n".to_vec();
    streamed.extend(vec![b' '; LIMIT + 1]);
    let invalid =
        b"HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nnot json".to_vec();
    let empty = b"HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n".to_vec();
    let (url, server) = server(vec![declared, streamed, invalid, empty]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    runtime.block_on(async {
        for _ in 0..2 {
            let error = gateway.list("demo").await.unwrap_err();
            assert!(error.to_string().contains("exceeds 4 MiB"), "{error:#}");
        }
        let error = gateway.list("demo").await.unwrap_err();
        assert!(error.to_string().contains("invalid JSON"), "{error:#}");
        assert_eq!(gateway.list("demo").await.unwrap(), Value::Null);
    });
    assert_eq!(server.join().unwrap().len(), 4);
}

#[test]
fn invalid_or_unreachable_gateways_fail_without_claiming_acceptance() {
    for url in [
        "not a url",
        "ftp://gateway.example",
        "https://user:secret@gateway.example",
        "https://gateway.example/?key=1",
        "https://gateway.example/#fragment",
        "https://gateway.example/prefix",
    ] {
        assert!(Gateway::new(url, "test-only".into()).is_err(), "{url}");
    }
    // Reserve a port, then release it so nothing is listening there.
    let port = TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port();
    let gateway = Gateway::new(&format!("http://127.0.0.1:{port}"), "test-only".into()).unwrap();
    let actor = agent(Provider::Codex);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    runtime.block_on(async {
        for project in ["", ".", "..", "a/b", "https://x", "demo?x=1"] {
            let error = gateway.list(project).await.unwrap_err();
            assert!(
                error.to_string().contains("project identifier"),
                "{project}"
            );
        }
        let error = gateway.claim("demo", TASK, &actor).await.unwrap_err();
        assert!(
            format!("{error:#}").contains("no acceptance or claim is established"),
            "{error:#}"
        );
        let error = gateway
            .request(Method::GET, "demo", &[TASK], None, None)
            .await
            .unwrap_err();
        assert!(format!("{error:#}").contains("gateway unavailable"));
    });
}

#[test]
fn last_task_owner_is_the_newest_event_snapshot_that_still_had_one() {
    let owner = json!({"session_id":"11111111-1111-4111-8111-111111111111",
        "instance_id":"22222222-2222-4222-8222-222222222222","provider":"claude","os":"linux"});
    let older = json!({"session_id":"33333333-3333-4333-8333-333333333333",
        "instance_id":"44444444-4444-4444-8444-444444444444","provider":"codex","os":"windows"});
    // Newest first: the user review snapshot has no owner, the one before does.
    let events = json!([
        {"id":3,"kind":"task_updated","task":{"id":TASK,"status":"user_review","owner_origin":null}},
        {"id":2,"kind":"task_commented","task":{"id":TASK,"status":"in_progress","owner_origin":owner}},
        {"id":1,"kind":"task_claimed","task":{"id":TASK,"status":"in_progress","owner_origin":older}}
    ]);
    let (url, server) = server(vec![reply(200, &events), reply(200, &json!([]))]);
    let gateway = Gateway::new(&url, "fixture".into()).unwrap();
    let id = uuid::Uuid::parse_str(TASK).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let last = gateway.last_task_owner("demo", id).await.unwrap().unwrap();
        assert_eq!(serde_json::to_value(last).unwrap(), owner);
        assert!(gateway.last_task_owner("demo", id).await.unwrap().is_none());
    });
    let requests = server.join().unwrap();
    assert!(requests[0].head.starts_with("get /v1/task-events?"));
    assert!(requests[0].head.contains(&format!("task_id={TASK}")));
    assert!(requests[0].head.contains("project=demo"));
}
