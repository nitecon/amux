//! Exact-session dispatch client against a local mock gateway. No live gateway,
//! credential provisioning or executor launch.
use amux::{
    dispatch::{ActionArgs, Dispatcher, Executor, Machine},
    identity::{AgentIdentity, Provider},
};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::{
    io::{ErrorKind, Read, Write},
    net::TcpListener,
    path::{Path, PathBuf},
    thread,
    time::{Duration, Instant},
};
use uuid::Uuid;

const MACHINE_KEY: &str = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90";

struct Seen {
    head: String,
    body: Value,
}

/// Replies in order; `None` closes the connection without answering. Returns
/// what it saw and its listener, so tests can assert nothing more was sent.
fn server(
    replies: Vec<Option<(u16, Value)>>,
) -> (String, thread::JoinHandle<(Vec<Seen>, TcpListener)>) {
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
            seen.push(Seen {
                head: String::from_utf8_lossy(&bytes[..end]).to_lowercase(),
                body: serde_json::from_slice(&bytes[end..end + length]).unwrap_or(Value::Null),
            });
            if let Some((status, body)) = reply {
                let body = serde_json::to_vec(&body).unwrap();
                let _ = write!(
                    stream,
                    "HTTP/1.1 {status} Test\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = stream.write_all(&body);
            }
        }
        (seen, listener)
    });
    (url, handle)
}

fn finish(server: thread::JoinHandle<(Vec<Seen>, TcpListener)>) -> Vec<Seen> {
    let (seen, listener) = server.join().unwrap();
    assert!(
        matches!(listener.accept(), Err(e) if e.kind() == ErrorKind::WouldBlock),
        "client sent an unexpected extra request"
    );
    seen
}

fn state_dir() -> (tempfile::TempDir, PathBuf) {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let state = fixture.path().join("state");
    (fixture, state)
}

fn machine() -> Machine {
    Machine::new(Uuid::new_v4(), MACHINE_KEY.into()).unwrap()
}

fn credential_id() -> String {
    Sha256::digest(MACHINE_KEY.as_bytes())
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

fn executor(state: &Path, provider: Provider) -> Executor {
    let actor = AgentIdentity::new(provider, Path::new(env!("CARGO_MANIFEST_DIR"))).unwrap();
    Executor::load(state, actor).unwrap()
}

fn dispatcher(url: &str, machine: Machine, state: &Path) -> Dispatcher {
    Dispatcher::new(url, "test-only".into(), "demo", machine, state.into()).unwrap()
}

fn session_key(state: &Path, executor: &Executor) -> String {
    std::fs::read_to_string(state.join("dispatch/keys").join(format!(
        "{}-{}.key",
        executor.actor.session_id, executor.actor.instance_id
    )))
    .unwrap()
}

/// A gateway acknowledgment for `me`, as the f060414 gateway returns it.
fn ack(me: &Value, task: Uuid, task_status: &str, dispatch: Value) -> Value {
    let mut full = json!({
        "task_id": task.to_string(),
        "project_ident": "demo",
        "source_project_ident": "demo",
        "requester": me,
        "target": me,
        "state": "pending",
        "claim_id": null,
        "received_at": null,
        "claimed_at": null,
        "started_at": null,
        "finished_at": null,
        "execution_id": null,
        "cancel_requested_at": null,
        "cancel_reason": null,
        "result": null,
    });
    for (key, value) in dispatch.as_object().unwrap() {
        full[key] = value.clone();
    }
    json!({
        "dispatch": full,
        "task": {
            "id": task.to_string(),
            "project_ident": full["project_ident"],
            "status": task_status,
            "owner_origin": if task_status == "in_progress" { me["origin"].clone() } else { Value::Null },
        },
        "authentication": {
            "machine": {
                "project_ident": "demo",
                "instance_id": me["origin"]["instance_id"],
                "os": me["origin"]["os"],
                "credential_id": credential_id(),
            },
            "session": me["origin"],
            "generation": me["generation"],
        },
    })
}

#[test]
fn three_same_project_agents_register_with_their_own_generation_and_secret() {
    let (_fixture, state) = state_dir();
    let agents = [
        executor(&state, Provider::Codex),
        executor(&state, Provider::Codex),
        executor(&state, Provider::Claude),
    ];
    let machine = machine();
    let instance = machine.instance_id;
    let targets: Vec<Value> = agents.iter().map(|a| a.target(&machine)).collect();
    let mut replies: Vec<_> = targets
        .iter()
        .map(|t| {
            Some((
                200,
                json!({"target": t, "seen_at": 1, "expires_at": 120_001}),
            ))
        })
        .collect();
    // A registration acknowledged for a different generation is refused.
    let mut wrong = targets[0].clone();
    wrong["generation"] = json!(Uuid::new_v4().to_string());
    replies.push(Some((
        200,
        json!({"target": wrong, "seen_at": 1, "expires_at": 2}),
    )));
    let (url, server) = server(replies);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for agent in &agents {
        runtime.block_on(dispatcher.register(agent)).unwrap();
    }
    assert!(runtime.block_on(dispatcher.register(&agents[0])).is_err());
    let seen = finish(server);

    let keys: Vec<_> = agents.iter().map(|a| session_key(&state, a)).collect();
    assert_eq!(
        keys.iter().collect::<std::collections::HashSet<_>>().len(),
        3,
        "each generation has its own secret"
    );
    for (agent, (request, key)) in agents.iter().zip(seen.iter().zip(&keys)) {
        let head = &request.head;
        assert!(head.starts_with("post /v1/projects/demo/task-sessions "));
        assert!(head.contains(&format!("x-agent-session-id: {}", agent.actor.session_id)));
        assert!(head.contains(&format!("x-agent-instance-id: {instance}")));
        assert!(head.contains(&format!(
            "x-agent-provider: {}",
            agent.actor.provider.command()
        )));
        assert!(head.contains(&format!("x-gateway-machine-key: {MACHINE_KEY}")));
        assert!(head.contains(&format!("x-gateway-session-key: {key}")));
        assert_eq!(
            request.body["generation"],
            agent.actor.instance_id.to_string()
        );
        // The wrapper instance is the generation, never the machine instance.
        assert_ne!(agent.actor.instance_id, instance);
    }
    // Reloading the same execution reuses its persisted secret.
    let again = Executor::load(&state, agents[2].actor.clone()).unwrap();
    assert_eq!(session_key(&state, &again), keys[2]);
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let path = state.join("dispatch/keys").join(format!(
            "{}-{}.key",
            agents[0].actor.session_id, agents[0].actor.instance_id
        ));
        assert_eq!(
            std::fs::metadata(path).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }
}

#[test]
fn a_lost_delegation_reply_stays_durable_and_only_its_execution_replays_it() {
    let (_fixture, state) = state_dir();
    let requester = executor(&state, Provider::Claude);
    let later = executor(&state, Provider::Claude);
    let machine = machine();
    let me = requester.target(&machine);
    let destination = executor(&state, Provider::Codex).target(&machine);
    let task = Uuid::new_v4();
    let accepted = ack(&me, task, "todo", json!({"target": destination}));
    let (url, server) = server(vec![None, Some((200, accepted))]);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();

    let lost = runtime.block_on(dispatcher.delegate(
        &requester,
        "demo",
        destination.clone(),
        "Implement it",
        "Spec",
        Some("linux"),
        None,
    ));
    assert!(lost.is_err(), "a lost reply is not acceptance");
    let pending = dispatcher.uncertain();
    assert_eq!(pending.len(), 1);
    let id = pending[0].request_id;
    // A later generation may not adopt or resubmit the earlier request.
    assert!(runtime.block_on(dispatcher.replay(&later, id)).is_err());
    runtime.block_on(dispatcher.replay(&requester, id)).unwrap();
    assert!(dispatcher.uncertain().is_empty());
    assert!(
        runtime.block_on(dispatcher.replay(&requester, id)).is_err(),
        "an accepted request is never resent"
    );
    let seen = finish(server);
    assert_eq!(seen.len(), 2, "one send and one explicit replay");
    assert_eq!(
        seen[0].body, seen[1].body,
        "replay is byte-for-byte the same request"
    );
    assert_eq!(seen[0].body["request_id"], id.to_string());
    assert_eq!(seen[0].body["target"], destination);
    assert_eq!(
        seen[0].body["generation"],
        requester.actor.instance_id.to_string()
    );
    assert!(
        seen[0]
            .head
            .starts_with("post /v1/projects/demo/task-dispatches ")
    );
    // The request survives a restart of the client.
    drop(dispatcher);
    let reopened = Dispatcher::new(
        "http://127.0.0.1:9",
        "t".into(),
        "demo",
        self::machine(),
        state.clone(),
    )
    .unwrap();
    assert!(
        reopened.uncertain().is_empty(),
        "accepted status was durable"
    );
}

#[test]
fn acknowledgments_must_match_the_exact_target_task_machine_claim_and_state() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Codex);
    let machine = machine();
    let me = agent.target(&machine);
    let other = executor(&state, Provider::Codex).target(&machine);
    let task = Uuid::new_v4();
    let execution = Uuid::new_v4();
    let claimed = |extra: Value| {
        let mut fields = json!({"state": "claimed", "claim_id": "c1", "claimed_at": 3});
        for (k, v) in extra.as_object().unwrap() {
            fields[k] = v.clone();
        }
        ack(&me, task, "in_progress", fields)
    };
    let mutate = |mut reply: Value, path: &[&str], value: Value| {
        let mut slot = &mut reply;
        for key in path {
            slot = &mut slot[*key];
        }
        *slot = value;
        reply
    };
    let refusals = vec![
        claimed(json!({"task_id": Uuid::new_v4().to_string()})),
        mutate(
            claimed(json!({})),
            &["dispatch", "task_id"],
            json!(Uuid::nil().to_string()),
        ),
        claimed(json!({"claim_id": null})),
        claimed(json!({"target": other})),
        claimed(json!({"started_at": 5})),
        claimed(json!({"claimed_at": null})),
        mutate(
            claimed(json!({})),
            &["task", "project_ident"],
            json!("elsewhere"),
        ),
        mutate(claimed(json!({})), &["task", "status"], json!("todo")),
        mutate(
            claimed(json!({})),
            &["authentication", "generation"],
            json!(Uuid::new_v4().to_string()),
        ),
        mutate(
            claimed(json!({})),
            &["authentication", "machine", "project_ident"],
            json!("elsewhere"),
        ),
        mutate(
            claimed(json!({})),
            &["authentication", "machine", "credential_id"],
            json!("0".repeat(64)),
        ),
        mutate(
            claimed(json!({})),
            &["authentication", "machine", "os"],
            json!("windows"),
        ),
    ];
    let count = refusals.len();
    let mut replies: Vec<_> = refusals.into_iter().map(|r| Some((200, r))).collect();
    replies.push(Some((409, json!({"error": "claimed by another session"}))));
    replies.push(Some((200, claimed(json!({})))));
    let running = |execution_id: Uuid| {
        ack(
            &me,
            task,
            "in_progress",
            json!({"state": "running", "claim_id": "c1", "claimed_at": 3, "started_at": 9, "execution_id": execution_id.to_string()}),
        )
    };
    replies.push(Some((200, running(Uuid::new_v4()))));
    replies.push(Some((200, running(execution))));
    let (url, server) = server(replies);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for _ in 0..=count {
        assert!(
            runtime
                .block_on(dispatcher.act(&agent, task, "claim", ActionArgs::default()))
                .is_err()
        );
    }
    // Malformed acknowledgments stay uncertain; the 409 is a definite rejection.
    assert_eq!(dispatcher.uncertain().len(), count);
    let accepted = runtime
        .block_on(dispatcher.act(&agent, task, "claim", ActionArgs::default()))
        .unwrap();
    assert_eq!(accepted["dispatch"]["claim_id"], "c1");
    assert!(
        accepted["dispatch"]["started_at"].is_null(),
        "a claim is not a start"
    );
    let start = || ActionArgs {
        claim_id: Some("c1".into()),
        execution_id: Some(execution),
        ..Default::default()
    };
    assert!(
        runtime
            .block_on(dispatcher.act(&agent, task, "start", start()))
            .is_err(),
        "a different execution ID is not this start"
    );
    runtime
        .block_on(dispatcher.act(&agent, task, "start", start()))
        .unwrap();
    let seen = finish(server);
    assert!(seen[0].head.starts_with(&format!(
        "post /v1/projects/demo/task-dispatches/{task}/actions "
    )));
    assert_eq!(
        seen.last().unwrap().body["execution_id"],
        execution.to_string()
    );
}

#[test]
fn cancellation_acknowledgments_follow_the_final_cancelled_task_status() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Codex);
    let machine = machine();
    let me = agent.target(&machine);
    let task = Uuid::new_v4();
    let reason = "no longer needed";
    let cancelled = json!({"state": "cancelled", "finished_at": 8, "result": reason});
    let requested = json!({"state": "claimed", "claim_id": "c1", "claimed_at": 3, "cancel_requested_at": 7, "cancel_reason": reason});
    let replies = vec![
        // Pre-f060414 semantics (cancel finished the task as done) are refused.
        Some((200, ack(&me, task, "done", cancelled.clone()))),
        Some((200, ack(&me, task, "cancelled", cancelled))),
        // A cancellation request records intent; the task stays in progress.
        Some((200, ack(&me, task, "done", requested.clone()))),
        Some((200, ack(&me, task, "in_progress", requested))),
    ];
    let (url, server) = server(replies);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let args = |claim: Option<&str>| ActionArgs {
        claim_id: claim.map(String::from),
        result: Some(reason.into()),
        ..Default::default()
    };
    assert!(
        runtime
            .block_on(dispatcher.act(&agent, task, "cancel", args(None)))
            .is_err()
    );
    runtime
        .block_on(dispatcher.act(&agent, task, "cancel", args(None)))
        .unwrap();
    assert!(
        runtime
            .block_on(dispatcher.act(&agent, task, "request_cancel", args(Some("c1"))))
            .is_err()
    );
    runtime
        .block_on(dispatcher.act(&agent, task, "request_cancel", args(Some("c1"))))
        .unwrap();
    assert_eq!(finish(server).len(), 4);
}

#[test]
fn invalid_input_persists_and_sends_nothing() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Claude);
    let machine = machine();
    let me = agent.target(&machine);
    let other = executor(&state, Provider::Claude).target(&machine);
    let foreign = json!({"pending": [{"target": other}], "machine_dispatches": [], "prior_claims": [], "requested": []});
    let (url, server) = server(vec![Some((200, foreign))]);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let task = Uuid::new_v4();
    let invalid = [
        (task, "start", ActionArgs::default()),
        (
            task,
            "succeeded",
            ActionArgs {
                execution_id: Some(Uuid::new_v4()),
                ..Default::default()
            },
        ),
        (task, "cancel", ActionArgs::default()),
        (
            task,
            "handoff",
            ActionArgs {
                result: Some("reason".into()),
                ..Default::default()
            },
        ),
        (task, "adopt", ActionArgs::default()),
        (Uuid::nil(), "receive", ActionArgs::default()),
        (
            task,
            "start",
            ActionArgs {
                claim_id: Some("c1".into()),
                execution_id: Some(Uuid::nil()),
                ..Default::default()
            },
        ),
        (
            task,
            "claim",
            ActionArgs {
                claim_id: Some(" ".into()),
                ..Default::default()
            },
        ),
        (
            task,
            "failed",
            ActionArgs {
                execution_id: Some(Uuid::new_v4()),
                result: Some("x".repeat(65_537)),
                ..Default::default()
            },
        ),
    ];
    for (task, operation, args) in invalid {
        assert!(
            runtime
                .block_on(dispatcher.act(&agent, task, operation, args))
                .is_err(),
            "{operation}"
        );
    }
    assert!(
        runtime
            .block_on(dispatcher.delegate(
                &agent,
                "demo",
                me.clone(),
                &"t".repeat(513),
                "",
                None,
                None
            ))
            .is_err()
    );
    assert!(
        runtime
            .block_on(dispatcher.delegate(&agent, "../x", me.clone(), "title", "", None, None))
            .is_err()
    );
    assert!(
        runtime
            .block_on(dispatcher.delegate(
                &agent,
                "demo",
                json!({"origin": {}}),
                "title",
                "",
                None,
                None
            ))
            .is_err()
    );
    assert!(
        dispatcher.uncertain().is_empty(),
        "invalid input persists nothing"
    );
    assert!(
        runtime.block_on(dispatcher.recover(&agent)).is_err(),
        "pending work for another generation is refused"
    );
    let seen = finish(server);
    assert_eq!(seen.len(), 1, "only the recovery read was sent");
    assert!(seen[0].head.starts_with(&format!(
        "get /v1/projects/demo/task-dispatches/recovery?generation={} ",
        agent.actor.instance_id
    )));
}

#[test]
fn corrupt_oversized_or_locked_journals_are_preserved_and_refused() {
    let (_fixture, state) = state_dir();
    let journal = state.join("dispatch/journal.json");
    std::fs::create_dir_all(journal.parent().unwrap()).unwrap();
    let cases: [Vec<u8>; 3] = [
        b"not json".to_vec(),
        br#"{"version":2,"requests":[]}"#.to_vec(),
        vec![b' '; 2 * 1024 * 1024 + 1],
    ];
    for bytes in cases {
        std::fs::write(&journal, &bytes).unwrap();
        assert!(
            Dispatcher::new(
                "http://127.0.0.1:9",
                "t".into(),
                "demo",
                machine(),
                state.clone()
            )
            .is_err()
        );
        assert_eq!(std::fs::read(&journal).unwrap(), bytes, "never reset");
    }
    std::fs::remove_file(&journal).unwrap();
    let first = Dispatcher::new(
        "http://127.0.0.1:9",
        "t".into(),
        "demo",
        machine(),
        state.clone(),
    )
    .unwrap();
    assert!(
        Dispatcher::new(
            "http://127.0.0.1:9",
            "t".into(),
            "demo",
            machine(),
            state.clone()
        )
        .is_err(),
        "a second process may not share the journal"
    );
    drop(first);
}

/// Unix permissions make the journal directory unwritable; skipped as root.
#[cfg(unix)]
#[test]
fn a_request_that_cannot_be_persisted_is_never_sent() {
    use std::os::unix::fs::PermissionsExt;
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Codex);
    let machine = machine();
    let me = agent.target(&machine);
    let (url, server) = server(vec![]);
    let dispatcher = dispatcher(&url, machine, &state);
    let directory = state.join("dispatch");
    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o500)).unwrap();
    if std::fs::write(directory.join("probe"), b"").is_ok() {
        return; // Running as root; permissions cannot block the write.
    }
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let error = runtime
        .block_on(dispatcher.delegate(&agent, "demo", me, "title", "", None, None))
        .unwrap_err();
    std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700)).unwrap();
    assert!(
        format!("{error:#}").contains("nothing was sent"),
        "{error:#}"
    );
    assert!(finish(server).is_empty(), "zero HTTP requests");
}

#[test]
fn shipped_cli_refuses_dispatch_outside_an_amux_launched_agent() {
    let (fixture, state) = state_dir();
    let output = std::process::Command::new(env!("CARGO_BIN_EXE_amux"))
        .args([
            "dispatch",
            "--project",
            "demo",
            "--gateway-url",
            "http://127.0.0.1:9",
            "register",
        ])
        .env("AMUX_STATE_DIR", &state)
        .env("AMUX_GATEWAY_KEY", "test-only")
        .env("AMUX_GATEWAY_MACHINE_ID", Uuid::new_v4().to_string())
        .env("AMUX_GATEWAY_MACHINE_KEY", MACHINE_KEY)
        .env_remove("AMUX_AGENT_SESSION_ID")
        .current_dir(fixture.path())
        .output()
        .unwrap();
    assert!(!output.status.success());
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(
        !stderr.contains("gateway unavailable"),
        "refused before networking: {stderr}"
    );
    assert!(
        !state.join("dispatch").exists(),
        "no secret is created for an unverified caller"
    );
}
