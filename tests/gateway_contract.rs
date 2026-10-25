//! Black-box gateway client contract: wire identity, claim acknowledgment,
//! response bounds and honest failure. Uses a local mock; no live gateway.
use amux::{
    gateway::Gateway,
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
            "github.com/nitecon/amux",
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
            .advertise_task_target("demo", "github.com/nitecon/amux", &actor, target.generation)
            .await
            .unwrap();
        let directory = gateway
            .task_targets("demo", "github.com/nitecon/amux")
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
            .block_on(gateway.task_targets("demo", "github.com/nitecon/amux"))
            .is_err()
    );
    assert_eq!(server.join().unwrap().len(), 1);
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
    let (url, server) = server(vec![reply(200, &owned_by(&actor))]);
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
    assert_eq!(requests[0].body["owner_agent_id"], actor.display_name());
    assert!(
        requests[0]
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
