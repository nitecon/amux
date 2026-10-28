//! `done` and `release` confirm exact execution ownership with an authoritative
//! read before sending any mutation. This is a client preflight, not atomic
//! server authorization.
#![cfg(unix)]
use amux::identity::{AgentIdentity, live_agents};
use serde_json::{Value, json};
use std::{
    fs,
    io::{ErrorKind, Read, Write},
    net::TcpListener,
    os::unix::fs::PermissionsExt,
    path::Path,
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

const TASK: &str = "01a1659c-2411-7201-930a-bcd58dcd5dec";

/// Builds the gateway's task read for a given agent.
type Reply = fn(&AgentIdentity) -> Value;

/// Serves `replies` in order, then hands back the listener so the test can
/// check that no further request was queued.
fn server(replies: Vec<Value>) -> (String, thread::JoinHandle<(Vec<String>, TcpListener)>) {
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
                    Err(e) if e.kind() == ErrorKind::WouldBlock => {
                        if Instant::now() > deadline {
                            return (seen, listener);
                        }
                        thread::sleep(Duration::from_millis(5));
                    }
                    Err(e) => panic!("accept: {e}"),
                }
            };
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
            seen.push(String::from_utf8_lossy(&bytes).into_owned());
            let body = serde_json::to_vec(&reply).unwrap();
            let _ = write!(
                stream,
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            );
            let _ = stream.write_all(&body);
        }
        (seen, listener)
    });
    (url, handle)
}

fn task(status: &str, owner: Option<(String, String)>) -> Value {
    let mut task = json!({"id": TASK, "project_ident": "demo", "status": status});
    if let Some((session, instance)) = owner {
        task["owner_origin"] = json!({"session_id": session, "instance_id": instance});
    }
    task
}

fn exact(actor: &AgentIdentity) -> Option<(String, String)> {
    Some((actor.session_id.to_string(), actor.instance_id.to_string()))
}

/// Launches a fake codex through `amux agent codex`. Once the gateway reply is
/// known (it depends on the agent's identity), the fake runs `amux tasks <op>`.
fn run(
    op: &str,
    replies: impl FnOnce(&AgentIdentity) -> Vec<Value>,
) -> (bool, String, Vec<String>) {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    fs::write(
        root.join("codex"),
        "#!/bin/sh\nread -r url\namux tasks --gateway-url \"$url\" \"$OP\" \"$TASK\"\nresult=$?\nexit $result\n",
    )
    .unwrap();
    fs::set_permissions(root.join("codex"), fs::Permissions::from_mode(0o700)).unwrap();
    let path = std::env::join_paths(std::iter::once(root.to_path_buf()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .unwrap();
    let mut agent = Command::new(env!("CARGO_BIN_EXE_amux"))
        .args(["agent", "codex"])
        .current_dir(root)
        .env("PATH", path)
        .env("OP", op)
        .env("TASK", TASK)
        .env("AMUX_STATE_DIR", root.join("state"))
        .env("AMUX_GATEWAY_PROJECT", "demo")
        .env("AMUX_GATEWAY_KEY", "test-only")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let actor = loop {
        if let Some(record) = live_agents(&root.join("state")).unwrap().pop() {
            break record.identity;
        }
        if Instant::now() > deadline {
            let _ = agent.kill();
            panic!("agent did not register");
        }
        thread::sleep(Duration::from_millis(25));
    };
    let (url, server) = server(replies(&actor));
    writeln!(agent.stdin.as_mut().unwrap(), "{url}").unwrap();
    let output = agent.wait_with_output().unwrap();
    let (requests, listener) = server.join().unwrap();
    assert!(
        matches!(listener.accept(), Err(e) if e.kind() == ErrorKind::WouldBlock),
        "client sent a request after the expected ones"
    );
    (
        output.status.success(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
        requests,
    )
}

#[test]
fn done_and_release_refuse_without_exact_confirmed_ownership() {
    let refusals: [(&str, Reply); 7] = [
        ("unclaimed", |_| task("todo", None)),
        ("already done", |a| task("done", exact(a))),
        ("legacy claim without owner origin", |_| {
            task("in_progress", None)
        }),
        ("another session", |a| {
            task(
                "in_progress",
                Some((uuid::Uuid::new_v4().to_string(), a.instance_id.to_string())),
            )
        }),
        ("another instance", |a| {
            task(
                "in_progress",
                Some((a.session_id.to_string(), uuid::Uuid::new_v4().to_string())),
            )
        }),
        ("partial owner origin", |a| {
            let mut t = task("in_progress", None);
            t["owner_origin"] = json!({"session_id": a.session_id.to_string()});
            t
        }),
        ("wrong project", |a| {
            let mut t = task("in_progress", exact(a));
            t["project_ident"] = json!("other");
            t
        }),
    ];
    for op in ["done", "release"] {
        for (case, reply) in refusals {
            let (ok, stderr, requests) = run(op, |a| vec![reply(a)]);
            assert!(!ok, "{op} with {case} must be refused: {stderr}");
            assert_eq!(requests.len(), 1, "{op} with {case}: only the read is sent");
            assert!(
                requests[0].starts_with(&format!("GET /v1/projects/demo/tasks/{TASK}?comments=0 ")),
                "{op} with {case}: {}",
                requests[0]
            );
        }
    }
}

#[test]
fn exact_owner_completes_or_releases_after_the_read() {
    for (op, status) in [("done", "done"), ("release", "todo")] {
        let (ok, stderr, requests) = run(op, |a| {
            vec![task("in_progress", exact(a)), task(status, exact(a))]
        });
        assert!(ok, "{op}: {stderr}");
        assert_eq!(requests.len(), 2);
        assert!(
            requests[0].starts_with(&format!("GET /v1/projects/demo/tasks/{TASK}?comments=0 "))
        );
        assert!(requests[1].starts_with(&format!("PATCH /v1/projects/demo/tasks/{TASK} ")));
        assert!(requests[1].ends_with(&format!("{{\"status\":\"{status}\"}}")));
    }
}
