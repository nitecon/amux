//! Task identity comes from an amux assignment, independently of OS ancestry.
use amux::identity::{AgentIdentity, AgentRecord, Provider};
use std::{path::Path, process::Command};

fn run(state: &Path, actor: &AgentIdentity, selector: &str) -> std::process::Output {
    let mut command = Command::new(env!("CARGO_BIN_EXE_amux"));
    for name in actor.environment().keys() {
        command.env_remove(name);
    }
    command
        .env("AMUX_STATE_DIR", state)
        .args([
            "--project-dir",
            env!("CARGO_MANIFEST_DIR"),
            "--agent",
            selector,
            "whoami",
        ])
        .output()
        .unwrap()
}

#[test]
fn explicit_assignment_works_without_provider_environment_or_process_ancestry() {
    let base = Path::new(env!("CARGO_TARGET_TMPDIR")).join("assigned-identity");
    std::fs::create_dir_all(&base).unwrap();
    let state = tempfile::tempdir_in(base).unwrap();
    std::fs::create_dir(state.path().join("agents")).unwrap();
    let mut actors = Vec::new();
    for number in [1, 2] {
        let actor = AgentIdentity::for_terminal(
            Provider::Codex,
            Path::new(env!("CARGO_MANIFEST_DIR")),
            number,
        )
        .unwrap();
        // Assignment lookup must not need a live, queryable process.
        let record = AgentRecord {
            identity: actor.clone(),
            pid: 0,
            process_start: 0,
            process_generation: "unavailable".into(),
        };
        std::fs::write(
            state
                .path()
                .join("agents")
                .join(format!("{}.json", actor.session_id)),
            serde_json::to_vec(&record).unwrap(),
        )
        .unwrap();
        let output = run(state.path(), &actor, &actor.session_id.to_string());
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert_eq!(
            serde_json::from_slice::<AgentIdentity>(&output.stdout).unwrap(),
            actor
        );
        let named = run(state.path(), &actor, &actor.display_name());
        assert!(named.status.success());
        actors.push(actor);
    }
    assert_ne!(actors[0].name, actors[1].name);
    assert_ne!(actors[0].session_id, actors[1].session_id);
    assert!(
        !run(state.path(), &actors[0], "not-assigned")
            .status
            .success()
    );
    let again = run(state.path(), &actors[0], &actors[0].session_id.to_string());
    assert_eq!(
        serde_json::from_slice::<AgentIdentity>(&again.stdout)
            .unwrap()
            .name,
        actors[0].name
    );
}

#[test]
fn name_pool_is_500_unique_technology_names_with_owner_in_the_middle() {
    let names: Vec<_> = include_str!("../assets/agent_names.txt").lines().collect();
    assert_eq!(names.len(), 500);
    assert_eq!(names[250], "Will Hattingh");
    assert_eq!(
        names.iter().collect::<std::collections::HashSet<_>>().len(),
        500
    );
    let project = Path::new(env!("CARGO_MANIFEST_DIR"));
    let first = AgentIdentity::for_terminal(Provider::Codex, project, 1).unwrap();
    let later = AgentIdentity::for_terminal(Provider::Codex, project, 501).unwrap();
    assert_ne!(first.display_name(), later.display_name());
}
