//! Built-in terminal identity: deterministic `provider-number` sessions per
//! project, a persistent global terminal counter, safe stale-record replacement
//! and lowercase repository-name gateway registration.
use amux::{
    gateway::Gateway,
    identity::{AgentIdentity, AgentRecord, Provider, next_terminal_number_at},
};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    io::{Read, Write},
    net::TcpListener,
    path::Path,
    process::{Command, Stdio},
};

fn project() -> &'static Path {
    Path::new(env!("CARGO_MANIFEST_DIR"))
}

fn fixture_dir() -> tempfile::TempDir {
    let base = Path::new(env!("CARGO_TARGET_TMPDIR")).join("terminal-identity-fixtures");
    std::fs::create_dir_all(&base).unwrap();
    tempfile::tempdir_in(base).unwrap()
}

#[test]
fn terminal_sessions_are_deterministic_per_provider_number_and_project() {
    let first = AgentIdentity::for_terminal(Provider::Codex, project(), 1).unwrap();
    let again = AgentIdentity::for_terminal(Provider::Codex, project(), 1).unwrap();
    assert_eq!(first.agent_id, "codex-1");
    assert_eq!(first.session_id, again.session_id);
    assert_eq!(first.project_id, again.project_id);
    assert!(!first.project_id.is_nil());
    assert_ne!(
        first.instance_id, again.instance_id,
        "each launch is a new execution"
    );

    let claude = AgentIdentity::for_terminal(Provider::Claude, project(), 1).unwrap();
    let second = AgentIdentity::for_terminal(Provider::Codex, project(), 2).unwrap();
    assert_eq!(claude.agent_id, "claude-1");
    assert_ne!(claude.session_id, first.session_id);
    assert_ne!(second.session_id, first.session_id);

    // Outside this checkout, so it is a different project.
    let other = fixture_dir();
    git(other.path(), &["init", "--quiet"]);
    let elsewhere = AgentIdentity::for_terminal(Provider::Codex, other.path(), 1).unwrap();
    assert_ne!(elsewhere.project_id, first.project_id);
    assert_ne!(elsewhere.session_id, first.session_id);

    assert!(AgentIdentity::for_terminal(Provider::Codex, project(), 0).is_err());
}

#[test]
fn environment_carries_project_and_agent_and_legacy_values_default() {
    let actor = AgentIdentity::for_terminal(Provider::Claude, project(), 3).unwrap();
    let values = actor.environment();
    assert_eq!(values["AMUX_AGENT_ID"], "claude-3");
    assert_eq!(
        values["AMUX_AGENT_PROJECT_ID"],
        actor.project_id.to_string()
    );
    assert_eq!(AgentIdentity::from_values(&values).unwrap(), actor);

    let mut legacy: HashMap<_, _> = values.clone();
    legacy.remove("AMUX_AGENT_ID");
    legacy.remove("AMUX_AGENT_PROJECT_ID");
    let parsed = AgentIdentity::from_values(&legacy).unwrap();
    assert!(parsed.project_id.is_nil());
    assert!(parsed.agent_id.is_empty());

    let mut malformed = values;
    malformed.insert("AMUX_AGENT_PROJECT_ID".into(), "not-a-uuid".into());
    assert!(AgentIdentity::from_values(&malformed).is_err());

    // Records written before these fields existed still deserialize.
    let mut old = serde_json::to_value(&actor).unwrap();
    old.as_object_mut().unwrap().remove("project_id");
    old.as_object_mut().unwrap().remove("agent_id");
    let old: AgentIdentity = serde_json::from_value(old).unwrap();
    assert!(old.project_id.is_nil() && old.agent_id.is_empty());
}

/// Concurrent reservations use an isolated app state directory.
#[test]
fn terminal_numbers_are_global_persistent_and_unique_under_concurrency() {
    let state = fixture_dir();
    assert_eq!(next_terminal_number_at(state.path()).unwrap(), 1);
    assert_eq!(next_terminal_number_at(state.path()).unwrap(), 2);
    assert_eq!(
        std::fs::read_to_string(state.path().join("terminal-number")).unwrap(),
        "2"
    );
    let threads: Vec<_> = (0..8)
        .map(|_| {
            let state_path = state.path().to_path_buf();
            std::thread::spawn(move || {
                (0..5)
                    .map(|_| next_terminal_number_at(&state_path).unwrap())
                    .collect::<Vec<_>>()
            })
        })
        .collect();
    let mut numbers: Vec<u64> = threads
        .into_iter()
        .flat_map(|t| t.join().unwrap())
        .collect();
    numbers.sort();
    assert_eq!(numbers, (3..43).collect::<Vec<_>>());

    std::fs::write(state.path().join("terminal-number"), "garbage").unwrap();
    assert!(
        next_terminal_number_at(state.path()).is_err(),
        "a corrupt counter never reuses numbers"
    );
}

#[test]
fn registration_is_idempotent_refuses_live_and_replaces_only_proven_stale() {
    let state = fixture_dir();
    let actor = AgentIdentity::for_terminal(Provider::Codex, project(), 7).unwrap();
    let path = state
        .path()
        .join("agents")
        .join(format!("{}.json", actor.session_id));

    // A previous launch of the same terminal session has exited.
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", "helper_wait_for_stdin", "--nocapture"])
        .env("AMUX_TERMINAL_IDENTITY_HELPER", "1")
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .spawn()
        .unwrap();
    let previous = AgentRecord::inspect(actor.clone(), child.id()).unwrap();
    previous.register_at(state.path()).unwrap();
    previous.register_at(state.path()).unwrap();

    // While it runs, a relaunch with the same session is refused.
    let relaunch = AgentIdentity::for_terminal(Provider::Codex, project(), 7).unwrap();
    let current = AgentRecord::inspect(relaunch.clone(), std::process::id()).unwrap();
    let error = current.register_at(state.path()).unwrap_err().to_string();
    assert!(error.contains("codex-7"), "{error}");

    drop(child.stdin.take());
    child.wait().unwrap();
    current.register_at(state.path()).unwrap();
    let stored: AgentRecord = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    assert_eq!(stored.identity, relaunch);
    assert_eq!(stored.pid, std::process::id());

    // Uninspectable liveness is not proof of exit: keep the record.
    let mut unknown = stored.clone();
    unknown.process_generation = "unknown".into();
    std::fs::write(&path, serde_json::to_vec(&unknown).unwrap()).unwrap();
    assert!(previous.register_at(state.path()).is_err());

    // A record written in a superseded generation format cannot be compared, so
    // it must not hold a session ID that this build can never release. Its own
    // exited PID is the proof of exit instead.
    let mut superseded = previous.clone();
    superseded.process_generation = "a format this build replaced".into();
    std::fs::write(&path, serde_json::to_vec(&superseded).unwrap()).unwrap();
    current.register_at(state.path()).unwrap();
    let stored: AgentRecord = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    assert_eq!(stored.pid, std::process::id());

    std::fs::write(&path, "not a record").unwrap();
    assert!(current.register_at(state.path()).is_err());
}

#[test]
fn helper_wait_for_stdin() {
    if std::env::var_os("AMUX_TERMINAL_IDENTITY_HELPER").is_some() {
        let _ = std::io::stdin().read_to_end(&mut Vec::new());
    }
}

fn git(directory: &Path, args: &[&str]) {
    let mut command = Command::new("git");
    command.arg("-C").arg(directory).args(args);
    for (key, _) in std::env::vars_os() {
        if key.to_string_lossy().starts_with("GIT_") {
            command.env_remove(key);
        }
    }
    assert!(
        command
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .status()
            .unwrap()
            .success()
    );
}

/// Answer each scripted reply on its own connection and return the request bodies.
fn serve(listener: TcpListener, replies: Vec<Value>) -> Vec<Value> {
    let mut bodies = Vec::new();
    for reply in replies {
        let (mut stream, _) = listener.accept().unwrap();
        let mut bytes = Vec::new();
        let mut buf = [0; 4096];
        let body = loop {
            let count = stream.read(&mut buf).unwrap();
            assert_ne!(count, 0);
            bytes.extend_from_slice(&buf[..count]);
            let Some(end) = bytes.windows(4).position(|w| w == b"\r\n\r\n") else {
                continue;
            };
            let head = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
            let length: usize = head
                .lines()
                .find_map(|l| l.strip_prefix("content-length: ")?.parse().ok())
                .unwrap_or(0);
            if bytes.len() >= end + 4 + length {
                break serde_json::from_slice(&bytes[end + 4..end + 4 + length])
                    .unwrap_or(Value::Null);
            }
        };
        bodies.push(body);
        let reply = serde_json::to_vec(&reply).unwrap();
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            reply.len()
        )
        .unwrap();
        stream.write_all(&reply).unwrap();
    }
    bodies
}

#[test]
fn new_gateway_project_is_registered_by_lowercase_repository_name() {
    let repo = fixture_dir();
    git(repo.path(), &["init", "--quiet"]);
    git(
        repo.path(),
        &[
            "remote",
            "add",
            "origin",
            "git@Git.Example.test:Team/SRE.git",
        ],
    );
    let remote = "git.example.test/team/sre";
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let server = std::thread::spawn(move || {
        serve(
            listener,
            vec![
                json!([]),
                json!({"ident": "sre"}),
                json!([{"ident": "sre", "canonical_remote": remote}]),
            ],
        )
    });
    let gateway = Gateway::new(&url, "test-only".into()).unwrap();
    let project = tokio::runtime::Runtime::new()
        .unwrap()
        .block_on(gateway.resolve_project(repo.path().into()))
        .unwrap();
    assert_eq!(project, "sre");
    let bodies = server.join().unwrap();
    assert_eq!(
        bodies[1],
        json!({"ident": "sre", "repo_url": format!("https://{remote}")})
    );
}
