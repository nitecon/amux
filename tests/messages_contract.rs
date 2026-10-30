#[path = "support/identity.rs"]
mod fixture_identity;
#[path = "support/native_cli.rs"]
mod native_cli;
use amux::{gateway::Gateway, identity::Provider};
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

#[test]
fn inline_messages_preserve_text_identity_and_uncertain_publication() {
    let (url, server) = server(vec![
        (200, json!({"threads":[],"counts":{}})),
        (
            200,
            json!({"root":{"id":42,"content":"handoff"},"replies":[]}),
        ),
        (
            200,
            json!({"message_id":42,"external_message_id":"external-42"}),
        ),
        (200, json!({"external_message_id":"missing-canonical-id"})),
        (503, json!({"error":"notifier unavailable"})),
    ]);
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let actor = fixture_identity::identity(
        Provider::Codex,
        std::path::Path::new(env!("CARGO_MANIFEST_DIR")),
    )
    .unwrap();
    let body = "handoff\nUnicode: λ; literal quotes: \"text\"; shell: $(never executed)";
    let runtime = tokio::runtime::Runtime::new().unwrap();
    runtime.block_on(async {
        assert!(gateway.messages("demo", 2, 5).await.unwrap()["threads"].is_array());
        assert_eq!(gateway.message("demo", 42).await.unwrap()["root"]["id"], 42);
        assert_eq!(
            gateway
                .send_message("demo", body.into(), Some("Review".into()), &actor)
                .await
                .unwrap()["message_id"],
            42
        );
        let error = gateway
            .send_message("demo", "next".into(), None, &actor)
            .await
            .unwrap_err();
        assert!(error.to_string().contains("publication is uncertain"));
        let error = gateway
            .send_message("demo", "third".into(), None, &actor)
            .await
            .unwrap_err();
        assert!(error.to_string().contains("503"));
        // These invalid inputs must not create any additional request.
        assert!(gateway.messages("demo", 501, 0).await.is_err());
        assert!(gateway.message("demo", 0).await.is_err());
        assert!(
            gateway
                .send_message("demo", "  ".into(), None, &actor)
                .await
                .is_err()
        );
        assert!(
            gateway
                .send_message("demo", "x".repeat(1024 * 1024), None, &actor)
                .await
                .is_err()
        );
    });
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 5, "no publication retry");
    assert!(
        requests[0]
            .headers
            .starts_with("GET /v1/projects/demo/messages?state=all&limit=2&offset=5 ")
    );
    assert!(
        requests[1]
            .headers
            .starts_with("GET /v1/projects/demo/messages/42/thread ")
    );
    for request in &requests[..2] {
        assert!(
            !request
                .headers
                .to_lowercase()
                .contains("x-agent-session-id:")
        );
    }
    assert_eq!(requests[2].body["body"], body);
    assert_eq!(requests[2].body["subject"], "Review");
    for request in &requests[2..] {
        let headers = request.headers.to_lowercase();
        assert!(headers.starts_with("post /v1/projects/demo/messages "));
        assert!(headers.contains(&format!("x-agent-session-id: {}", actor.session_id)));
        assert!(headers.contains(&format!("x-agent-instance-id: {}", actor.instance_id)));
        assert!(headers.contains("authorization: bearer test-only"));
    }
}

#[test]
fn shipped_cli_reads_threads_and_refuses_unwrapped_publication() {
    let (url, server) = server(vec![(200, json!({"root":{"id":42},"replies":[]}))]);
    let state = tempfile::tempdir().unwrap();
    let native = native_cli::NativeCli::new(state.path(), &url);
    let url = native.url.clone();
    let args = ["messages", "--project", "demo", "--gateway-url", &url];
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .env("AMUX_STATE_DIR", state.path())
        .args(args)
        .args(["get", "42"])
        .env("AMUX_GATEWAY_KEY", "test-only")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        serde_json::from_slice::<Value>(&output.stdout).unwrap()["root"]["id"],
        42
    );
    assert_eq!(server.join().unwrap().len(), 1);
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .env("AMUX_STATE_DIR", state.path())
        .args(args)
        .args(["send", "must not publish"])
        .env("AMUX_GATEWAY_KEY", "test-only")
        .env_remove("AMUX_AGENT_SESSION_ID")
        .output()
        .unwrap();
    assert!(!output.status.success());
    assert!(
        !String::from_utf8_lossy(&output.stderr).contains("gateway unavailable"),
        "publication must fail before networking"
    );
}

#[cfg(unix)]
#[test]
fn launched_agent_publishes_with_its_registered_execution_identity() {
    use std::{
        fs,
        os::unix::fs::PermissionsExt,
        path::Path,
        process::{Child, Stdio},
    };
    struct Agent(Child);
    impl Drop for Agent {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    fs::write(
        root.join("codex"),
        "#!/bin/sh\nread -r go\namux messages send \"$HANDOFF_BODY\" --subject Fixture\n",
    )
    .unwrap();
    fs::set_permissions(root.join("codex"), fs::Permissions::from_mode(0o700)).unwrap();
    let path = std::env::join_paths(std::iter::once(root.to_path_buf()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .unwrap();
    let (url, server) = server(vec![(
        200,
        json!({"message_id":81,"external_message_id":"external-81"}),
    )]);
    let _native = native_cli::NativeCli::new(&root.join("state"), &url);
    let body = "handoff\n$(do not execute)";
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
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
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
        assert!(Instant::now() < deadline, "publication did not finish");
        thread::sleep(Duration::from_millis(25));
    }
    let requests = server.join().unwrap();
    assert_eq!(requests.len(), 1);
    assert_eq!(requests[0].body["body"], body);
    let headers = requests[0].headers.to_lowercase();
    assert!(headers.contains(&format!(
        "x-agent-id: {}",
        record.identity.display_name().to_lowercase()
    )));
    assert!(headers.contains(&format!(
        "x-agent-session-id: {}",
        record.identity.session_id
    )));
    assert!(headers.contains(&format!(
        "x-agent-instance-id: {}",
        record.identity.instance_id
    )));
    assert!(headers.contains("x-agent-provider: codex"));
}
