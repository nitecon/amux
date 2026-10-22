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
        "status": "in_progress",
        "owner_origin": {
            "session_id": actor.session_id.to_string(),
            "instance_id": actor.instance_id.to_string(),
        }
    })
}

fn agent(provider: Provider) -> AgentIdentity {
    AgentIdentity::new(provider, Path::new(env!("CARGO_MANIFEST_DIR"))).unwrap()
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
        assert!(head.contains(&format!("x-agent-id: {}", actor.session_id)));
        assert!(head.contains(&format!("x-agent-session-id: {}", actor.session_id)));
        assert!(head.contains(&format!("x-agent-instance-id: {}", actor.instance_id)));
        assert!(head.contains(&format!("x-agent-provider: {}", actor.provider.command())));
        assert!(head.contains(&format!("x-agent-os: {}", actor.os)));
        assert_eq!(request.body["status"], "in_progress");
        assert_eq!(request.body["owner_agent_id"], actor.session_id.to_string());
        // No request may carry a peer's identity.
        for peer in agents.iter().filter(|p| *p != actor) {
            assert!(!head.contains(&peer.session_id.to_string()));
            assert!(!head.contains(&peer.instance_id.to_string()));
        }
    }
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
    assert_eq!(server.join().unwrap().len(), refusals.len(), "no retries");
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
