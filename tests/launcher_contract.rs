//! Exercise actual subprocess launches rather than manufacturing three identities.
#![cfg(unix)]
use std::{
    collections::HashSet,
    fs,
    io::Write,
    os::unix::fs::PermissionsExt,
    path::Path,
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};

struct Agent(Child);
impl Drop for Agent {
    fn drop(&mut self) {
        drop(self.0.stdin.take());
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn launch(root: &Path) -> Agent {
    let mut command = Command::new(env!("CARGO_BIN_EXE_amux"));
    let path = std::env::join_paths(std::iter::once(root.to_path_buf()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .unwrap();
    command
        .args(["agent", "codex"])
        .current_dir(root)
        .env("PATH", path)
        .env("AMUX_STATE_DIR", root.join("state"))
        .env("AMUX_CAPTURE_DIR", root.join("captures"))
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped());
    Agent(command.spawn().unwrap())
}

#[test]
fn three_live_same_project_agents_are_distinct_and_restart_does_not_reuse_a_claim_identity() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    fs::create_dir(root.join("captures")).unwrap();
    let fake = root.join("codex");
    fs::write(&fake, "#!/bin/sh\namux whoami > \"$AMUX_CAPTURE_DIR/$AMUX_AGENT_SESSION_ID.json\"\nread -r stop\n").unwrap();
    fs::set_permissions(&fake, fs::Permissions::from_mode(0o700)).unwrap();
    let mut agents: Vec<_> = (0..3).map(|_| launch(root)).collect();
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        let records = amux::identity::live_agents(&root.join("state")).unwrap();
        let captures = fs::read_dir(root.join("captures")).unwrap().count();
        if records.len() == 3 && captures == 3 {
            break;
        }
        for child in &mut agents {
            assert!(
                child.0.try_wait().unwrap().is_none(),
                "agent launcher exited unexpectedly"
            );
        }
        assert!(
            Instant::now() < deadline,
            "three sessions failed to register"
        );
        std::thread::sleep(Duration::from_millis(25));
    }
    let records = amux::identity::live_agents(&root.join("state")).unwrap();
    let sessions: HashSet<_> = records.iter().map(|r| r.identity.session_id).collect();
    let instances: HashSet<_> = records.iter().map(|r| r.identity.instance_id).collect();
    assert_eq!(sessions.len(), 3);
    assert_eq!(instances.len(), 3);
    for record in &records {
        let captured: amux::identity::AgentIdentity = serde_json::from_slice(
            &fs::read(
                root.join("captures")
                    .join(format!("{}.json", record.identity.session_id)),
            )
            .unwrap(),
        )
        .unwrap();
        assert_eq!(captured, record.identity);
        assert_eq!(
            captured.project_directory,
            amux::project_identity::resolve(root).unwrap().directory
        );
    }
    for child in &mut agents {
        child
            .0
            .stdin
            .as_mut()
            .unwrap()
            .write_all(b"stop\n")
            .unwrap();
        assert!(child.0.wait().unwrap().success());
    }
    assert!(
        amux::identity::live_agents(&root.join("state"))
            .unwrap()
            .is_empty()
    );
    // Recovery retains native proof after clean exit, without advertising any
    // stopped executor as live or letting a new launch inherit its identity.
    for record in &records {
        assert!(
            root.join("state/agents")
                .join(format!("{}.json", record.identity.session_id))
                .exists()
        );
        assert!(amux::identity::process_is_gone(
            record.pid,
            &record.process_generation
        ));
    }
    let mut restarted = launch(root);
    let deadline = Instant::now() + Duration::from_secs(10);
    let next = loop {
        if let Some(record) = amux::identity::live_agents(&root.join("state"))
            .unwrap()
            .pop()
        {
            break record;
        }
        assert!(Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(25));
    };
    assert!(!sessions.contains(&next.identity.session_id));
    assert!(!instances.contains(&next.identity.instance_id));
    restarted
        .0
        .stdin
        .as_mut()
        .unwrap()
        .write_all(b"stop\n")
        .unwrap();
    assert!(restarted.0.wait().unwrap().success());
}

#[test]
fn restored_agent_launches_forward_native_continuation_arguments_without_a_prompt() {
    use amux::identity::Provider;
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    for (provider, expected) in [
        (Provider::Codex, "resume\n--last\n"),
        (Provider::Claude, "--continue\n"),
    ] {
        let script = root.join(provider.command());
        fs::write(
            &script,
            "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AMUX_ARGS_FILE\"\nread -r stop\n",
        )
        .unwrap();
        fs::set_permissions(&script, fs::Permissions::from_mode(0o700)).unwrap();
        let log = root.join(format!("{}.args", provider.command()));
        let path = std::env::join_paths(std::iter::once(root.to_path_buf()).chain(
            std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
        ))
        .unwrap();
        let mut child = Agent(
            Command::new(env!("CARGO_BIN_EXE_amux"))
                .args(["agent", provider.command(), "--"])
                .args(amux::session::resume_arguments(provider))
                .current_dir(root)
                .env("PATH", path)
                .env("AMUX_STATE_DIR", root.join("state"))
                .env("AMUX_ARGS_FILE", &log)
                .stdin(Stdio::piped())
                .stdout(Stdio::null())
                .stderr(Stdio::piped())
                .spawn()
                .unwrap(),
        );
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            if fs::read_to_string(&log).is_ok_and(|text| text == expected) {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "provider did not receive native resume arguments"
            );
            std::thread::sleep(Duration::from_millis(20));
        }
        child
            .0
            .stdin
            .as_mut()
            .unwrap()
            .write_all(b"stop\n")
            .unwrap();
        assert!(child.0.wait().unwrap().success());
    }
}
