#[path = "support/identity.rs"]
mod fixture_identity;
use amux::{
    gateway::Gateway,
    identity::{AgentIdentity, Provider},
};
use serde_json::{Value, json};
use std::{
    io::{Read, Write},
    net::{TcpListener, TcpStream},
    process::Command,
    thread,
    time::{Duration, Instant},
};

struct Request {
    headers: String,
    body: Value,
}

fn server(replies: Vec<(u16, Value)>) -> (String, thread::JoinHandle<Vec<Request>>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    listener.set_nonblocking(true).unwrap();
    let handle = thread::spawn(move || {
        let mut requests = Vec::new();
        for (status, reply) in replies {
            let deadline = Instant::now() + Duration::from_secs(15);
            let mut stream: TcpStream = loop {
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
            // Only accept is polled; request reads use the bounded blocking timeout.
            stream.set_nonblocking(false).unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let mut bytes = Vec::new();
            let (header_end, length) = loop {
                let mut buf = [0; 4096];
                let count = stream.read(&mut buf).unwrap();
                assert_ne!(count, 0, "request closed before headers");
                bytes.extend_from_slice(&buf[..count]);
                if let Some(end) = bytes.windows(4).position(|w| w == b"\r\n\r\n") {
                    let header = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                    let length = header
                        .lines()
                        .find_map(|line| {
                            line.strip_prefix("content-length: ")
                                .map(|v| v.parse::<usize>().unwrap())
                        })
                        .unwrap_or(0);
                    break (end + 4, length);
                }
                assert!(bytes.len() < 16 * 1024);
            };
            while bytes.len() < header_end + length {
                let mut buf = [0; 4096];
                let count = stream.read(&mut buf).unwrap();
                assert_ne!(count, 0, "incomplete request body");
                bytes.extend_from_slice(&buf[..count]);
            }
            requests.push(Request {
                headers: String::from_utf8(bytes[..header_end].to_vec()).unwrap(),
                body: if length == 0 {
                    Value::Null
                } else {
                    serde_json::from_slice(&bytes[header_end..header_end + length]).unwrap()
                },
            });
            let body = serde_json::to_vec(&reply).unwrap();
            write!(stream, "HTTP/1.1 {status} Test\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n", body.len()).unwrap();
            stream.write_all(&body).unwrap();
        }
        requests
    });
    (url, handle)
}

const TASK: &str = "01a16514-9144-7e32-b6b0-4af81b44b0b8";

fn report(actor: &AgentIdentity) -> Value {
    let origin = json!({"session_id":actor.session_id,"instance_id":actor.instance_id,"provider":actor.provider.command(),"os":actor.os});
    json!({"task_id":TASK,"project_ident":"demo","reason":"Stop <literal> & preserve history\nrequested by user","prior_status":"in_progress","prior_owner_agent_id":"same-host-label","prior_owner_origin":origin,"prior_started_at":42,"reporter_origin":origin,"cancelled_at":123,"executor_stop_confirmed":false})
}

fn acknowledgment(record: &Value) -> Value {
    json!({"task":{"id":TASK,"project_ident":"demo","status":"cancelled","owner_agent_id":record["prior_owner_agent_id"],"owner_origin":record["prior_owner_origin"],"started_at":record["prior_started_at"],"done_at":null},"cancellation":record})
}

#[test]
fn cancellation_preserves_provenance_and_rejects_false_acknowledgments() {
    let actor = fixture_identity::identity(
        Provider::Codex,
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")),
    )
    .unwrap();
    let record = report(&actor);
    let good = acknowledgment(&record);
    let mut replies = vec![(200, good.clone()), (200, record.clone())];
    for (path, replacement) in [
        ("/task/status", json!("done")),
        ("/task/id", json!(uuid::Uuid::new_v4())),
        ("/task/project_ident", json!("foreign")),
        ("/task/owner_origin/session_id", json!(uuid::Uuid::new_v4())),
        ("/task/started_at", json!(43)),
        ("/task/done_at", json!(123)),
        ("/cancellation/task_id", json!(uuid::Uuid::new_v4())),
        ("/cancellation/project_ident", json!("foreign")),
        (
            "/cancellation/reporter_origin/session_id",
            json!(uuid::Uuid::new_v4()),
        ),
        ("/cancellation/reporter_origin/provider", json!("claude")),
        ("/cancellation/executor_stop_confirmed", json!(true)),
        ("/cancellation/cancelled_at", json!(0)),
        ("/cancellation/prior_status", json!("done")),
        ("/cancellation/reason", json!("different reason")),
    ] {
        let mut bad = good.clone();
        *bad.pointer_mut(path).unwrap() = replacement;
        replies.push((200, bad));
    }
    replies.extend([
        (200, Value::Null),
        (200, json!({})),
        (409, json!({"error":"another session owns this task"})),
    ]);
    let bad_count = replies.len() - 2;
    let (url, handle) = server(replies);
    let gateway = Gateway::new(&url, "fixture-only".into()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    runtime.block_on(async {
        let reason = record["reason"].as_str().unwrap();
        assert_eq!(
            gateway
                .cancel_task(
                    "demo",
                    TASK.parse().unwrap(),
                    &format!("  {reason}  "),
                    &actor
                )
                .await
                .unwrap(),
            good
        );
        assert_eq!(
            gateway
                .cancellation("demo", TASK.parse().unwrap())
                .await
                .unwrap(),
            record
        );
        for _ in 0..bad_count {
            assert!(
                gateway
                    .cancel_task("demo", TASK.parse().unwrap(), reason, &actor)
                    .await
                    .is_err()
            );
        }
    });
    let requests = handle.join().unwrap();
    assert_eq!(requests.len(), bad_count + 2);
    assert!(requests[0].headers.starts_with(&format!(
        "POST /v1/projects/demo/tasks/{TASK}/cancellation "
    )));
    assert_eq!(requests[0].body, json!({"reason":record["reason"]}));
    let headers = requests[0].headers.to_lowercase();
    assert!(headers.contains(&format!("x-agent-session-id: {}", actor.session_id)));
    assert!(headers.contains(&format!("x-agent-instance-id: {}", actor.instance_id)));
    assert!(requests[1].headers.starts_with("GET "));
    assert!(
        !requests[1]
            .headers
            .to_lowercase()
            .contains("x-agent-session-id")
    );
}

#[test]
fn cancellation_reads_reject_foreign_or_unbound_reports() {
    let actor = fixture_identity::identity(
        Provider::Codex,
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")),
    )
    .unwrap();
    let record = report(&actor);
    let mut replies = Vec::new();
    for (field, value) in [
        ("task_id", json!(uuid::Uuid::new_v4())),
        ("project_ident", json!("foreign")),
        ("prior_owner_origin", Value::Null),
        ("reporter_origin", Value::Null),
        ("executor_stop_confirmed", Value::Null),
    ] {
        let mut bad = record.clone();
        bad[field] = value;
        replies.push((200, bad));
    }
    let count = replies.len();
    let (url, handle) = server(replies);
    let gateway = Gateway::new(&url, "fixture-only".into()).unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        for _ in 0..count {
            assert!(
                gateway
                    .cancellation("demo", TASK.parse().unwrap())
                    .await
                    .is_err()
            );
        }
    });
    assert_eq!(handle.join().unwrap().len(), count);
}

#[test]
fn invalid_cancellation_and_unwrapped_cli_never_contact_gateway() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let gateway = Gateway::new(&url, "fixture-only".into()).unwrap();
    let actor = fixture_identity::identity(
        Provider::Codex,
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")),
    )
    .unwrap();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        for reason in ["".to_owned(), " \n ".to_owned(), "x".repeat(16_385)] {
            assert!(
                gateway
                    .cancel_task("demo", TASK.parse().unwrap(), &reason, &actor)
                    .await
                    .is_err()
            );
        }
        assert!(
            gateway
                .cancel_task("demo", uuid::Uuid::nil(), "reason", &actor)
                .await
                .is_err()
        );
        assert!(
            gateway
                .cancellation("demo", uuid::Uuid::nil())
                .await
                .is_err()
        );
    });
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .args([
            "tasks",
            "--project",
            "demo",
            "--gateway-url",
            &url,
            "cancel",
            TASK,
            "--reason",
            "requested",
        ])
        .env("AMUX_GATEWAY_KEY", "fixture-only")
        .env_remove("AMUX_AGENT_SESSION_ID")
        .env_remove("AMUX_AGENT_INSTANCE_ID")
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(
        listener.accept().is_err(),
        "invalid or unwrapped cancellation reached gateway"
    );
}

#[test]
fn shipped_cli_reads_cancellation_without_claim_or_agent_identity() {
    let actor = fixture_identity::identity(
        Provider::Codex,
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")),
    )
    .unwrap();
    let record = report(&actor);
    let (url, handle) = server(vec![(200, record.clone())]);
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .args([
            "tasks",
            "--project",
            "demo",
            "--gateway-url",
            &url,
            "cancellation",
            TASK,
        ])
        .env("AMUX_GATEWAY_KEY", "fixture-only")
        .env_remove("AMUX_AGENT_SESSION_ID")
        .env_remove("AMUX_AGENT_INSTANCE_ID")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        serde_json::from_slice::<Value>(&output.stdout).unwrap(),
        record
    );
    let requests = handle.join().unwrap();
    assert_eq!(requests.len(), 1);
    assert!(
        requests[0]
            .headers
            .starts_with(&format!("GET /v1/projects/demo/tasks/{TASK}/cancellation "))
    );
    assert!(!requests[0].headers.to_lowercase().contains("x-agent-"));
}
