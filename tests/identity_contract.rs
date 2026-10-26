//! Identity parsing and native process-generation fences. Launcher, caller
//! ancestry and lifecycle cases live in their own contract tests.
use amux::identity::{AgentIdentity, AgentRecord, Provider};
use std::{collections::HashMap, path::Path};

fn agent(provider: Provider) -> AgentIdentity {
    AgentIdentity::for_terminal(provider, Path::new(env!("CARGO_MANIFEST_DIR")), {
        static NUMBER: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);
        NUMBER.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
    })
    .unwrap()
}

#[test]
fn launch_environment_round_trips_and_malformed_identity_is_refused() {
    for provider in [Provider::Codex, Provider::Claude] {
        let actor = agent(provider);
        assert_eq!(
            AgentIdentity::from_values(&actor.environment()).unwrap(),
            actor
        );
    }
    let valid = agent(Provider::Codex).environment();
    let nil = uuid::Uuid::nil().to_string();
    let cases: [(&str, Option<&str>); 9] = [
        ("AMUX_AGENT_SESSION_ID", None),
        ("AMUX_AGENT_INSTANCE_ID", None),
        ("AMUX_AGENT_PROJECT", None),
        ("AMUX_AGENT_PROVIDER", Some("gemini")),
        ("AMUX_AGENT_OS", Some("plan9")),
        ("AMUX_AGENT_SESSION_ID", Some("not-a-uuid")),
        ("AMUX_AGENT_INSTANCE_ID", Some("not-a-uuid")),
        ("AMUX_AGENT_SESSION_ID", Some(&nil)),
        ("AMUX_AGENT_INSTANCE_ID", Some(&nil)),
    ];
    for (name, value) in cases {
        let mut values: HashMap<String, String> = valid.clone();
        match value {
            Some(v) => values.insert(name.into(), v.into()),
            None => values.remove(name),
        };
        assert!(
            AgentIdentity::from_values(&values).is_err(),
            "{name}={value:?} must be refused"
        );
    }
}

#[test]
fn terminal_slots_are_distinct_even_for_one_project_and_provider() {
    let agents: Vec<_> = (0..3).map(|_| agent(Provider::Claude)).collect();
    let mut ids: Vec<_> = agents
        .iter()
        .flat_map(|a| [a.session_id, a.instance_id])
        .collect();
    ids.sort();
    ids.dedup();
    assert_eq!(ids.len(), 6, "session and instance IDs are never shared");
}

/// Helper child: waits until the parent closes its stdin, then exits.
#[test]
fn helper_wait_for_stdin() {
    if std::env::var_os("AMUX_IDENTITY_HELPER").is_some() {
        let _ = std::io::Read::read_to_end(&mut std::io::stdin(), &mut Vec::new());
    }
}

#[cfg(any(target_os = "linux", target_os = "macos", windows))]
#[test]
fn process_generation_fences_a_registration_to_one_native_process() {
    use amux::identity::process_generation;
    use std::process::{Command, Stdio};

    let own = process_generation(std::process::id()).unwrap();
    assert_eq!(own, process_generation(std::process::id()).unwrap());
    let prefix = if cfg!(windows) {
        "windows:"
    } else if cfg!(target_os = "macos") {
        "macos:"
    } else {
        "linux:"
    };
    assert!(own.starts_with(prefix), "{own}");

    let mut child = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", "helper_wait_for_stdin", "--nocapture"])
        .env("AMUX_IDENTITY_HELPER", "1")
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    // The fence is PID plus generation: start ticks alone can coincide with
    // another process started in the same clock tick.
    let record = AgentRecord::inspect(agent(Provider::Codex), child.id()).unwrap();
    assert!(record.is_live(&sysinfo::System::new_all()));

    // A matching PID with a different generation is a reused PID, not this agent.
    let mut reused = record.clone();
    reused.process_generation = format!("{prefix}not-this-process");
    assert!(!reused.is_live(&sysinfo::System::new_all()));

    drop(child.stdin.take());
    assert!(child.wait().unwrap().success());
    assert!(
        !record.is_live(&sysinfo::System::new_all()),
        "an exited agent must not keep its registration live"
    );
}
