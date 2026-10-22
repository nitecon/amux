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

fn receipt() -> Value {
    json!({
        "source_task":{"id":"11111111-1111-4111-8111-111111111111","project_ident":"demo","status":"todo","kind":"delegated","delegated_to_project_ident":"other","delegated_to_task_id":"22222222-2222-4222-8222-222222222222"},
        "target_task":{"id":"22222222-2222-4222-8222-222222222222","project_ident":"other","status":"todo"},
        "delegation":{"id":"33333333-3333-4333-8333-333333333333","source_project_ident":"demo","target_project_ident":"other","source_task_id":"11111111-1111-4111-8111-111111111111","target_task_id":"22222222-2222-4222-8222-222222222222"},
        "message_id":42
    })
}

#[test]
fn delegation_preserves_inline_context_and_requires_the_requested_linked_records() {
    let good = receipt();
    let mut replies = vec![(200, good.clone())];
    for (path, replacement) in [
        ("/source_task/id", Value::Null),
        ("/target_task/id", json!(uuid::Uuid::nil())),
        ("/source_task/project_ident", json!("wrong")),
        ("/target_task/project_ident", json!("wrong")),
        ("/source_task/status", json!("unknown")),
        ("/delegation/id", json!("not-a-uuid")),
        (
            "/delegation/target_task_id",
            json!("11111111-1111-4111-8111-111111111111"),
        ),
        ("/delegation/source_project_ident", json!("other")),
        ("/source_task/delegated_to_task_id", Value::Null),
        ("/message_id", json!(0)),
    ] {
        let mut bad = good.clone();
        *bad.pointer_mut(path).unwrap() = replacement;
        replies.push((200, bad));
    }
    replies.push((200, Value::Null));
    replies.push((409, json!({"error":"refused"})));
    let count = replies.len();
    let (url, server) = server(replies);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let actor = AgentIdentity::new(Provider::Codex, &std::env::current_dir().unwrap()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let context = "line one\n$(literal text) <b>context</b>";
    assert_eq!(
        runtime
            .block_on(gateway.delegate(
                "demo",
                "other",
                "  Review  ".into(),
                Some(context.into()),
                &actor
            ))
            .unwrap(),
        good
    );
    for _ in 1..count {
        let error = runtime
            .block_on(gateway.delegate(
                "demo",
                "other",
                "Review".into(),
                Some(context.into()),
                &actor,
            ))
            .unwrap_err();
        assert!(error.to_string().contains("inspect"), "{error:#}");
    }
    let requests = server.join().unwrap();
    assert_eq!(
        requests.len(),
        count,
        "no retry is allowed after an unconfirmed delegation"
    );
    for request in requests {
        assert!(
            request
                .headers
                .starts_with("POST /v1/projects/demo/tasks/delegate HTTP/1.1")
        );
        assert_eq!(
            request.body,
            json!({"target_project_ident":"other","title":"Review","specification":context})
        );
        let headers = request.headers.to_lowercase();
        assert!(headers.contains(&format!("x-agent-session-id: {}", actor.session_id)));
        assert!(headers.contains(&format!("x-agent-instance-id: {}", actor.instance_id)));
        assert!(headers.contains("x-agent-provider: codex"));
    }
}

#[test]
fn invalid_delegations_and_unregistered_cli_callers_send_nothing() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let actor = AgentIdentity::new(Provider::Claude, &std::env::current_dir().unwrap()).unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for (target, title, context) in [
        ("demo", "Review".into(), None),
        ("DEMO", "Review".into(), None),
        ("../other", "Review".into(), None),
        ("other", " ".into(), None),
        ("other", "a".repeat(1025), None),
        ("other", "Review\nnow".into(), None),
        ("other", "Review".into(), Some("x".repeat(65537))),
    ] {
        assert!(
            runtime
                .block_on(gateway.delegate("demo", target, title, context, &actor))
                .is_err()
        );
    }
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .args([
            "tasks",
            "--project",
            "demo",
            "--gateway-url",
            &url,
            "delegate",
            "Review",
            "--target-project",
            "other",
        ])
        .env("AMUX_GATEWAY_KEY", "test-only")
        .env_remove("AMUX_AGENT_SESSION_ID")
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(!String::from_utf8_lossy(&output.stderr).contains("gateway unavailable"));
    assert!(
        matches!(listener.accept(), Err(error) if error.kind() == std::io::ErrorKind::WouldBlock)
    );
}

#[cfg(unix)]
#[test]
fn registered_agent_cli_delegates_without_agent_tools() {
    use std::{fs, os::unix::fs::PermissionsExt, path::Path, process::Stdio};
    let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&fixtures).unwrap();
    let fixture = tempfile::tempdir_in(fixtures).unwrap();
    let root = fixture.path();
    fs::write(root.join("codex"), "#!/bin/sh\nread -r go\namux tasks delegate Review --target-project other --specification \"$HANDOFF_BODY\"\n").unwrap();
    fs::set_permissions(root.join("codex"), fs::Permissions::from_mode(0o700)).unwrap();
    // Only the fixture and basic shell tools: no agent-tools binary is available.
    let path = std::env::join_paths([root, Path::new("/usr/bin"), Path::new("/bin")]).unwrap();
    let (url, server) = server(vec![(200, receipt())]);
    let body = "inline\n$(must stay literal)";
    struct Agent(std::process::Child);
    impl Drop for Agent {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
    let mut agent = Agent(
        Command::new(env!("CARGO_BIN_EXE_amux"))
            .args(["agent", "codex"])
            .current_dir(root)
            .env("PATH", path)
            .env("HANDOFF_BODY", body)
            .env("AMUX_STATE_DIR", root.join("state"))
            .env("AMUX_GATEWAY_PROJECT", "demo")
            .env("AMUX_GATEWAY_URL", url)
            .env("AMUX_GATEWAY_KEY", "test-only")
            .stdin(std::process::Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap(),
    );
    let deadline = Instant::now() + Duration::from_secs(10);
    let record = loop {
        if let Some(record) = amux::identity::live_agents(&root.join("state"))
            .unwrap()
            .pop()
        {
            break record;
        }
        assert!(Instant::now() < deadline, "agent did not register");
        thread::sleep(Duration::from_millis(25));
    };
    agent.0.stdin.as_mut().unwrap().write_all(b"go\n").unwrap();
    let deadline = Instant::now() + Duration::from_secs(15);
    loop {
        if let Some(status) = agent.0.try_wait().unwrap() {
            let mut error = String::new();
            agent
                .0
                .stderr
                .take()
                .unwrap()
                .read_to_string(&mut error)
                .unwrap();
            assert!(status.success(), "{error}");
            break;
        }
        assert!(Instant::now() < deadline, "delegation did not finish");
        thread::sleep(Duration::from_millis(25));
    }
    let mut output = Vec::new();
    agent
        .0
        .stdout
        .take()
        .unwrap()
        .read_to_end(&mut output)
        .unwrap();
    assert_eq!(serde_json::from_slice::<Value>(&output).unwrap(), receipt());
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 1);
    assert_eq!(requests[0].body["specification"], body);
    let headers = requests[0].headers.to_lowercase();
    assert!(headers.contains(&format!(
        "x-agent-session-id: {}",
        record.identity.session_id
    )));
    assert!(headers.contains(&format!(
        "x-agent-instance-id: {}",
        record.identity.instance_id
    )));
}
