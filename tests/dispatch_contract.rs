//! Exact-session dispatch client against a local mock gateway. No live gateway,
//! credential provisioning or executor launch.
use amux::{
    dispatch::{ActionArgs, Destination, Dispatcher, Executor, Machine},
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

/// This machine's OS, as the gateway would have provisioned it.
const OS: &str = std::env::consts::OS;

/// A supported OS that is not this one, for mismatch fixtures.
fn other_os() -> &'static str {
    if OS == "windows" { "linux" } else { "windows" }
}

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
        Destination::Session(destination.clone()),
        "Implement it",
        "Spec",
        Some("linux"),
        None,
    ));
    assert!(lost.is_err(), "a lost reply is not acceptance");
    let pending = dispatcher.uncertain().unwrap();
    assert_eq!(pending.len(), 1);
    let id = pending[0].request_id;
    // A later generation may not adopt or resubmit the earlier request.
    assert!(runtime.block_on(dispatcher.replay(&later, id)).is_err());
    runtime.block_on(dispatcher.replay(&requester, id)).unwrap();
    assert!(dispatcher.uncertain().unwrap().is_empty());
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
        reopened.uncertain().unwrap().is_empty(),
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
            json!(other_os()),
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
    for case in 0..=count {
        let result = runtime.block_on(dispatcher.act(&agent, task, "claim", ActionArgs::default()));
        assert!(
            result.is_err(),
            "refusal case {case} was accepted: {result:?}"
        );
    }
    // Malformed acknowledgments stay uncertain; the 409 is a definite rejection.
    assert_eq!(dispatcher.uncertain().unwrap().len(), count);
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
                Destination::Session(me.clone()),
                &"t".repeat(513),
                "",
                None,
                None
            ))
            .is_err()
    );
    assert!(
        runtime
            .block_on(dispatcher.delegate(
                &agent,
                "../x",
                Destination::Session(me.clone()),
                "title",
                "",
                None,
                None
            ))
            .is_err()
    );
    assert!(
        runtime
            .block_on(dispatcher.delegate(
                &agent,
                "demo",
                Destination::Session(json!({"origin": {}})),
                "title",
                "",
                None,
                None
            ))
            .is_err()
    );
    assert!(
        dispatcher.uncertain().unwrap().is_empty(),
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
    // A process that holds the journal lock past the bounded wait makes a
    // write fail closed, and nothing is sent.
    let agent = executor(&state, Provider::Codex);
    let machine = machine();
    let me = agent.target(&machine);
    let (url, server) = server(vec![]);
    let dispatcher = dispatcher(&url, machine, &state);
    let holder = std::fs::OpenOptions::new()
        .write(true)
        .open(state.join("dispatch/journal.lock"))
        .unwrap();
    holder.try_lock().unwrap();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let error = runtime
        .block_on(dispatcher.delegate(
            &agent,
            "demo",
            Destination::Session(me),
            "title",
            "",
            None,
            None,
        ))
        .unwrap_err();
    assert!(format!("{error:#}").contains("busy"), "{error:#}");
    drop(holder);
    assert!(finish(server).is_empty(), "zero HTTP requests");
    assert!(dispatcher.uncertain().unwrap().is_empty());
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
        .block_on(dispatcher.delegate(
            &agent,
            "demo",
            Destination::Session(me),
            "title",
            "",
            None,
            None,
        ))
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

/// A pending_machine acknowledgment: no bound session, provisioned OS from the server.
fn queued(me: &Value, task: Uuid, instance: Uuid, os: &str) -> Value {
    ack(
        me,
        task,
        "todo",
        json!({"state": "pending_machine", "target": null, "machine_target": {"instance_id": instance.to_string(), "os": os}}),
    )
}

#[test]
fn work_queues_for_an_offline_machine_without_changing_session_requests() {
    let (_fixture, state) = state_dir();
    let requester = executor(&state, Provider::Claude);
    let machine = machine();
    let me = requester.target(&machine);
    let windows = Uuid::new_v4();
    let task = Uuid::new_v4();
    let mut bound = queued(&me, task, windows, "windows");
    bound["dispatch"]["target"] = me.clone();
    let replies = vec![
        Some((200, queued(&me, task, Uuid::new_v4(), "windows"))), // another machine
        Some((200, queued(&me, task, windows, "linux"))),          // not the required OS
        Some((
            200,
            ack(
                &me,
                task,
                "todo",
                json!({"machine_target": {"instance_id": windows.to_string(), "os": "windows"}}),
            ),
        )), // pending, not pending_machine
        Some((200, bound)),                                        // already bound to a session
        Some((
            409,
            json!({"error": "target machine is not provisioned/eligible in project"}),
        )),
        Some((200, queued(&me, task, windows, "windows"))),
        Some((200, ack(&me, task, "todo", json!({})))), // exact-session delegation
    ];
    let (url, server) = server(replies);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let to_machine = || {
        dispatcher.delegate(
            &requester,
            "demo",
            Destination::Machine(windows),
            "Run on Windows",
            "Spec",
            Some("windows"),
            Some("codex"),
        )
    };
    for _ in 0..5 {
        assert!(runtime.block_on(to_machine()).is_err());
    }
    assert_eq!(
        dispatcher.uncertain().unwrap().len(),
        4,
        "the 409 is a definite rejection"
    );
    let accepted = runtime.block_on(to_machine()).unwrap();
    assert_eq!(accepted["dispatch"]["state"], "pending_machine");
    assert!(
        accepted["dispatch"]["target"].is_null(),
        "no session is bound or claimed"
    );
    runtime
        .block_on(dispatcher.delegate(
            &requester,
            "demo",
            Destination::Session(me.clone()),
            "Here",
            "",
            None,
            None,
        ))
        .unwrap();
    let seen = finish(server);
    let machine_body = &seen[5].body;
    assert_eq!(
        machine_body["machine_target"],
        json!({"instance_id": windows.to_string()})
    );
    assert!(
        machine_body.get("target").is_none(),
        "exactly one destination"
    );
    let session_body = &seen[6].body;
    assert!(
        session_body.get("machine_target").is_none(),
        "exact-session request bodies (and their replay fingerprints) are unchanged"
    );
    assert_eq!(session_body["target"], me);
}

#[test]
fn a_machine_item_is_bound_only_by_explicit_receive_and_one_session_wins() {
    let (_fixture, state) = state_dir();
    let first = executor(&state, Provider::Codex);
    let second = executor(&state, Provider::Codex);
    let machine = machine();
    let instance = machine.instance_id;
    let me = first.target(&machine);
    let task = Uuid::new_v4();
    let received = |target: &Value, instance: Uuid| {
        ack(
            target,
            task,
            "todo",
            json!({"state": "received", "received_at": 4, "machine_target": {"instance_id": instance.to_string(), "os": OS}}),
        )
    };
    let recovery = |items: Vec<Value>| json!({"pending": [], "machine_pending": items, "machine_dispatches": [], "prior_claims": [], "requested": []});
    let mut other_machine = queued(&me, task, Uuid::new_v4(), OS)["dispatch"].clone();
    other_machine["requester"] = me.clone();
    let mut already_bound = queued(&me, task, instance, OS)["dispatch"].clone();
    already_bound["target"] = me.clone();
    let replies = vec![
        Some((
            200,
            recovery(vec![queued(&me, task, instance, OS)["dispatch"].clone()]),
        )),
        Some((200, recovery(vec![other_machine]))),
        Some((200, recovery(vec![already_bound]))),
        Some((200, received(&me, Uuid::new_v4()))), // receipt for another machine
        Some((200, received(&me, instance))),
        Some((409, json!({"error": "ownership conflict"}))), // second session loses
    ];
    let (url, server) = server(replies);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let pending = runtime.block_on(dispatcher.recover(&first)).unwrap();
    assert_eq!(pending["machine_pending"].as_array().unwrap().len(), 1);
    assert!(
        runtime.block_on(dispatcher.recover(&first)).is_err(),
        "another machine's work"
    );
    assert!(
        runtime.block_on(dispatcher.recover(&first)).is_err(),
        "already bound work"
    );
    assert!(
        runtime
            .block_on(dispatcher.act(&first, task, "receive", ActionArgs::default()))
            .is_err()
    );
    let bound = runtime
        .block_on(dispatcher.act(&first, task, "receive", ActionArgs::default()))
        .unwrap();
    assert_eq!(
        bound["dispatch"]["target"], me,
        "receipt binds this exact session"
    );
    assert!(
        bound["dispatch"]["claim_id"].is_null(),
        "receipt is not a claim"
    );
    assert_eq!(bound["task"]["status"], "todo");
    let error = runtime
        .block_on(dispatcher.act(&second, task, "receive", ActionArgs::default()))
        .unwrap_err();
    assert!(format!("{error:#}").contains("409"), "{error:#}");
    let seen = finish(server);
    assert!(
        seen[0]
            .head
            .starts_with("get /v1/projects/demo/task-dispatches/recovery")
    );
    assert!(
        seen[4].body.get("machine_target").is_none(),
        "receive names no destination"
    );
}

#[test]
fn owners_can_hand_off_to_a_machine_and_destinations_are_exclusive() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Codex);
    let machine = machine();
    let me = agent.target(&machine);
    let windows = Uuid::new_v4();
    let task = Uuid::new_v4();
    let reason = "needs Windows";
    let handed = |instance: Uuid| {
        let mut reply = queued(&me, task, instance, "windows");
        reply["dispatch"]["result"] = json!(reason);
        reply
    };
    let (url, server) = server(vec![
        Some((200, handed(Uuid::new_v4()))),
        Some((200, handed(windows))),
    ]);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let handoff = |target: Option<Value>, machine_target: Option<Uuid>| ActionArgs {
        claim_id: Some("c1".into()),
        result: Some(reason.into()),
        target,
        machine_target,
        ..Default::default()
    };
    let invalid = [
        ("handoff", handoff(Some(me.clone()), Some(windows))),
        ("handoff", handoff(None, Some(Uuid::nil()))),
        ("handoff", handoff(None, None)),
        ("claim", handoff(None, Some(windows))),
    ];
    for (operation, args) in invalid {
        assert!(
            runtime
                .block_on(dispatcher.act(&agent, task, operation, args))
                .is_err()
        );
    }
    assert!(
        dispatcher.uncertain().unwrap().is_empty(),
        "invalid destinations persist nothing"
    );
    assert!(
        runtime
            .block_on(dispatcher.act(&agent, task, "handoff", handoff(None, Some(windows))))
            .is_err(),
        "queued for a different machine"
    );
    let queued = runtime
        .block_on(dispatcher.act(&agent, task, "handoff", handoff(None, Some(windows))))
        .unwrap();
    assert_eq!(queued["dispatch"]["state"], "pending_machine");
    assert!(
        queued["dispatch"]["claim_id"].is_null(),
        "the old claim is cleared"
    );
    let seen = finish(server);
    assert_eq!(seen.len(), 2);
    assert_eq!(
        seen[1].body["machine_target"],
        json!({"instance_id": windows.to_string()})
    );
    assert!(seen[1].body["target"].is_null());
}

#[test]
fn independent_clients_share_the_journal_while_one_call_is_stalled() {
    use std::{
        collections::HashMap,
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
    };
    let (_fixture, state) = state_dir();
    let agents = [
        executor(&state, Provider::Codex),
        executor(&state, Provider::Codex),
        executor(&state, Provider::Claude),
    ];
    let machine_id = Uuid::new_v4();
    let machine = || Machine::new(machine_id, MACHINE_KEY.into()).unwrap();
    let targets: Vec<Value> = agents.iter().map(|a| a.target(&machine())).collect();
    let destination = targets[0].clone();
    let replies: HashMap<String, Value> = agents
        .iter()
        .zip(&targets)
        .map(|(agent, me)| {
            let reply = ack(me, Uuid::new_v4(), "todo", json!({"target": destination}));
            (agent.actor.session_id.to_string(), reply)
        })
        .collect();
    let stalled = agents[0].actor.session_id.to_string();
    // Concurrent mock gateway: the first agent's call waits until both peers
    // have been answered.
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let answered = Arc::new(AtomicUsize::new(0));
    let gateway = {
        let answered = answered.clone();
        thread::spawn(move || {
            let mut handlers = Vec::new();
            for _ in 0..3 {
                let (mut stream, _) = listener.accept().unwrap();
                let (replies, stalled, answered) =
                    (replies.clone(), stalled.clone(), answered.clone());
                handlers.push(thread::spawn(move || {
                    stream.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
                    let mut bytes = Vec::new();
                    let (end, length) = loop {
                        let mut buf = [0; 4096];
                        let count = stream.read(&mut buf).unwrap();
                        bytes.extend_from_slice(&buf[..count]);
                        if let Some(end) = bytes.windows(4).position(|w| w == b"\r\n\r\n") {
                            let head = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                            let length = head
                                .lines()
                                .find_map(|l| l.strip_prefix("content-length: ")?.parse().ok())
                                .unwrap_or(0usize);
                            break (end + 4, length);
                        }
                    };
                    while bytes.len() < end + length {
                        let mut buf = [0; 4096];
                        let count = stream.read(&mut buf).unwrap();
                        bytes.extend_from_slice(&buf[..count]);
                    }
                    let head = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                    let session = head
                        .lines()
                        .find_map(|l| l.strip_prefix("x-agent-session-id: "))
                        .unwrap()
                        .to_owned();
                    if session == stalled {
                        let deadline = Instant::now() + Duration::from_secs(10);
                        while answered.load(Ordering::SeqCst) < 2 {
                            assert!(Instant::now() < deadline, "peers were blocked by the stalled call");
                            thread::sleep(Duration::from_millis(10));
                        }
                    }
                    let body = serde_json::to_vec(&replies[&session]).unwrap();
                    write!(
                        stream,
                        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    )
                    .unwrap();
                    stream.write_all(&body).unwrap();
                    stream.flush().unwrap();
                    if session != stalled {
                        answered.fetch_add(1, Ordering::SeqCst);
                    }
                }));
            }
            for handler in handlers {
                handler.join().unwrap();
            }
        })
    };
    // Three independent clients, as three CLI processes would create them.
    let clients: Vec<Dispatcher> = (0..3)
        .map(|_| dispatcher(&url, machine(), &state))
        .collect();
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let call = |i: usize| {
        clients[i].delegate(
            &agents[i],
            "demo",
            Destination::Session(destination.clone()),
            "Parallel work",
            "",
            None,
            None,
        )
    };
    let results = runtime.block_on(async {
        let (a, b, c) = tokio::join!(call(0), call(1), call(2));
        [a, b, c]
    });
    gateway.join().unwrap();
    for result in results {
        result.unwrap();
    }
    assert_eq!(answered.load(Ordering::SeqCst), 2);
    // All three outcomes survive in the shared journal, none lost to a peer's write.
    let journal: Value =
        serde_json::from_slice(&std::fs::read(state.join("dispatch/journal.json")).unwrap())
            .unwrap();
    let requests = journal["requests"].as_array().unwrap();
    assert_eq!(requests.len(), 3);
    assert!(requests.iter().all(|r| r["status"] == "accepted"));
}

#[test]
fn unclaimed_acknowledgments_carry_no_claim_execution_outcome_or_owner() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Codex);
    let machine = machine();
    let instance = machine.instance_id;
    let me = agent.target(&machine);
    let task = Uuid::new_v4();
    let reason = "needs this machine";
    let residue = [
        (["dispatch", "claim_id"], json!("c1")),
        (["dispatch", "claimed_at"], json!(3)),
        (["dispatch", "started_at"], json!(5)),
        (
            ["dispatch", "execution_id"],
            json!(Uuid::new_v4().to_string()),
        ),
        (["dispatch", "finished_at"], json!(8)),
        (["dispatch", "cancel_requested_at"], json!(7)),
        (["task", "owner_origin"], me["origin"].clone()),
        (["task", "owner_agent_id"], json!("someone")),
        (["task", "started_at"], json!(5)),
        (["task", "done_at"], json!(8)),
    ];
    let received = ack(
        &me,
        task,
        "todo",
        json!({"state": "received", "received_at": 4, "machine_target": {"instance_id": instance.to_string(), "os": OS}}),
    );
    let mut handed = queued(&me, task, instance, OS);
    handed["dispatch"]["result"] = json!(reason);
    let canonical = [queued(&me, task, instance, OS), received, handed];
    let mut replies = Vec::new();
    for reply in &canonical {
        for (path, value) in &residue {
            let mut bad = reply.clone();
            bad[path[0]][path[1]] = value.clone();
            replies.push(Some((200, bad)));
        }
        replies.push(Some((200, reply.clone())));
    }
    let (url, server) = server(replies);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    for step in 0..canonical.len() {
        let call = || match step {
            0 => runtime.block_on(dispatcher.delegate(
                &agent,
                "demo",
                Destination::Machine(instance),
                "Queued",
                "Spec",
                None,
                None,
            )),
            1 => runtime.block_on(dispatcher.act(&agent, task, "receive", ActionArgs::default())),
            _ => runtime.block_on(dispatcher.act(
                &agent,
                task,
                "handoff",
                ActionArgs {
                    claim_id: Some("c1".into()),
                    result: Some(reason.into()),
                    machine_target: Some(instance),
                    ..Default::default()
                },
            )),
        };
        for (case, (path, _)) in residue.iter().enumerate() {
            let result = call();
            assert!(
                result.is_err(),
                "step {step} residue {case} {path:?} was accepted: {result:?}"
            );
        }
        call().unwrap();
    }
    // Residue is never journaled as accepted; each stays uncertain.
    assert_eq!(
        dispatcher.uncertain().unwrap().len(),
        canonical.len() * residue.len()
    );
    finish(server);
}

/// A stored delegation from `executor` on `machine` through `url`.
fn stored(
    url: &str,
    machine: Uuid,
    executor: &Executor,
    status: &str,
    specification: usize,
) -> Value {
    let id = Uuid::new_v4();
    json!({
        "request_id": id,
        "gateway": format!("{url}/"),
        "project": "demo",
        "machine_instance": machine,
        "session_id": executor.actor.session_id,
        "generation": executor.generation(),
        "path": ["task-dispatches"],
        "body": {
            "request_id": id,
            "generation": executor.generation(),
            "target_project_ident": "demo",
            "target": executor.target(&Machine::new(machine, MACHINE_KEY.into()).unwrap()),
            "title": "seeded",
            "specification": "s".repeat(specification),
        },
        "status": status,
        "response": null,
        "error": null,
    })
}

fn seed(state: &Path, requests: &[Value]) -> PathBuf {
    let path = state.join("dispatch/journal.json");
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(
        &path,
        serde_json::to_vec(&json!({"version": 1, "requests": requests})).unwrap(),
    )
    .unwrap();
    path
}

fn journal_ids(path: &Path) -> Vec<String> {
    let journal: Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    journal["requests"]
        .as_array()
        .unwrap()
        .iter()
        .map(|r| r["request_id"].as_str().unwrap().to_owned())
        .collect()
}

#[test]
fn a_full_journal_drops_only_its_oldest_settled_request() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Claude);
    let machine = machine();
    let instance = machine.instance_id;
    let me = agent.target(&machine);
    let (url, server) = server(vec![None, None]);
    let mut requests = vec![
        stored(&url, instance, &agent, "accepted", 0),
        stored(&url, instance, &agent, "rejected", 0),
    ];
    for _ in 2..512 {
        requests.push(stored(&url, instance, &agent, "uncertain", 0));
    }
    let path = seed(&state, &requests);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();

    let lost = runtime.block_on(dispatcher.delegate(
        &agent,
        "demo",
        Destination::Session(me),
        "new work",
        "",
        None,
        None,
    ));
    assert!(lost.is_err(), "a lost reply is not acceptance");
    let ids = journal_ids(&path);
    assert_eq!(ids.len(), 512, "the journal stays bounded");
    let evicted = requests[0]["request_id"].as_str().unwrap();
    assert!(
        !ids.iter().any(|id| id == evicted),
        "oldest settled request dropped"
    );
    assert!(
        ids.iter()
            .any(|id| id == requests[1]["request_id"].as_str().unwrap())
    );
    for request in &requests[2..] {
        assert!(
            ids.contains(&request["request_id"].as_str().unwrap().to_owned()),
            "uncertain kept"
        );
    }
    assert_eq!(dispatcher.uncertain().unwrap().len(), 511);

    // An evicted ID is unknown and never resent.
    let error = runtime
        .block_on(dispatcher.replay(&agent, Uuid::parse_str(evicted).unwrap()))
        .unwrap_err();
    assert!(
        format!("{error:#}").contains("nothing was sent"),
        "{error:#}"
    );
    // A retained uncertain request still replays unchanged under its stable ID.
    let retained = Uuid::parse_str(requests[2]["request_id"].as_str().unwrap()).unwrap();
    assert!(
        runtime
            .block_on(dispatcher.replay(&agent, retained))
            .is_err()
    );
    let seen = finish(server);
    assert_eq!(seen.len(), 2, "one new request and one explicit replay");
    assert_eq!(seen[0].body["title"], "new work");
    assert_eq!(seen[1].body, requests[2]["body"]);
    assert_eq!(journal_ids(&path).len(), 512);
}

#[test]
fn a_journal_full_of_uncertain_requests_refuses_before_sending() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Codex);
    let machine = machine();
    let instance = machine.instance_id;
    let me = agent.target(&machine);
    let (url, server) = server(vec![]);
    let requests: Vec<_> = (0..512)
        .map(|_| stored(&url, instance, &agent, "uncertain", 0))
        .collect();
    let path = seed(&state, &requests);
    let original = std::fs::read(&path).unwrap();
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let error = runtime
        .block_on(dispatcher.delegate(
            &agent,
            "demo",
            Destination::Session(me),
            "t",
            "",
            None,
            None,
        ))
        .unwrap_err();
    assert!(
        format!("{error:#}").contains("nothing was sent"),
        "{error:#}"
    );
    assert_eq!(std::fs::read(&path).unwrap(), original, "journal unchanged");
    assert!(finish(server).is_empty(), "zero HTTP requests");
}

#[test]
fn the_byte_bound_also_drops_only_settled_requests() {
    let (_fixture, state) = state_dir();
    let agent = executor(&state, Provider::Claude);
    let machine = machine();
    let instance = machine.instance_id;
    let me = agent.target(&machine);
    let (url, server) = server(vec![None]);
    // One uncertain and 30 settled large requests fit under 2 MiB; one more does not.
    let mut requests: Vec<_> = (0..30)
        .map(|_| stored(&url, instance, &agent, "accepted", 66_000))
        .collect();
    requests.insert(0, stored(&url, instance, &agent, "uncertain", 66_000));
    let path = seed(&state, &requests);
    let dispatcher = dispatcher(&url, machine, &state);
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let specification = "n".repeat(65_536);
    assert!(
        runtime
            .block_on(dispatcher.delegate(
                &agent,
                "demo",
                Destination::Session(me),
                "big",
                &specification,
                None,
                None,
            ))
            .is_err()
    );
    assert_eq!(finish(server).len(), 1);
    let ids = journal_ids(&path);
    assert!(std::fs::metadata(&path).unwrap().len() <= 2 * 1024 * 1024);
    assert_eq!(
        ids[0],
        requests[0]["request_id"].as_str().unwrap(),
        "uncertain kept"
    );
    assert!(!ids.contains(&requests[1]["request_id"].as_str().unwrap().to_owned()));
    assert_eq!(ids.len(), 31, "exactly one settled request dropped");
}
