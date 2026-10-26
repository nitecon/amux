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

fn resume_fixture(root: &Path, provider: amux::identity::Provider) -> std::ffi::OsString {
    let script = root.join(provider.command());
    fs::write(
        &script,
        r#"#!/bin/sh
printf '%s:%s\n' "$#" "$*" >> "$AMUX_ARGS_FILE"
printf '%s %s %s\n' "$AMUX_AGENT_SESSION_ID" "$AMUX_AGENT_INSTANCE_ID" "$$" >> "$AMUX_IDENTITY_FILE"
if [ "$#" -gt 0 ]; then
    if [ "$AMUX_RESUME_EXIT" = signal ]; then kill -TERM $$; fi
    if [ "$AMUX_RESUME_EXIT" = 1 ]; then echo 'No conversation found' >&2; fi
    exit "$AMUX_RESUME_EXIT"
fi
if [ "$AMUX_FRESH_EXIT" = wait ]; then read -r stop; exit 0; fi
exit "$AMUX_FRESH_EXIT"
"#,
    )
    .unwrap();
    fs::set_permissions(script, fs::Permissions::from_mode(0o700)).unwrap();
    std::env::join_paths(
        std::iter::once(root.to_path_buf()).chain(std::env::split_paths(
            &std::env::var_os("PATH").unwrap_or_default(),
        )),
    )
    .unwrap()
}

#[test]
fn missing_history_restarts_both_providers_without_resume_args_and_stays_interactive() {
    use amux::{identity::Provider, session::resume_arguments};
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    for provider in [Provider::Codex, Provider::Claude] {
        let fixture = tempfile::tempdir_in(&target).unwrap();
        let root = fixture.path();
        let mut child = Agent(
            Command::new(env!("CARGO_BIN_EXE_amux"))
                .args(["agent", provider.command(), "--"])
                .args(resume_arguments(provider))
                .current_dir(root)
                .env("PATH", resume_fixture(root, provider))
                .env("AMUX_STATE_DIR", root.join("state"))
                .env("AMUX_ARGS_FILE", root.join("args"))
                .env("AMUX_IDENTITY_FILE", root.join("identities"))
                .env("AMUX_RESUME_EXIT", "1")
                .env("AMUX_FRESH_EXIT", "wait")
                .stdin(Stdio::piped())
                .stdout(Stdio::null())
                .stderr(Stdio::piped())
                .spawn()
                .unwrap(),
        );
        let expected = format!(
            "{}:{}\n0:\n",
            resume_arguments(provider).len(),
            resume_arguments(provider).join(" ")
        );
        let deadline = Instant::now() + Duration::from_secs(10);
        let records = loop {
            let records = amux::identity::live_agents(&root.join("state")).unwrap();
            if fs::read_to_string(root.join("args")).is_ok_and(|s| s == expected)
                && records.len() == 1
            {
                break records;
            }
            assert!(
                child.0.try_wait().unwrap().is_none(),
                "fallback exited instead of opening a fresh session"
            );
            assert!(
                Instant::now() < deadline,
                "fallback did not register and stay interactive"
            );
            std::thread::sleep(Duration::from_millis(20));
        };
        let identities = fs::read_to_string(root.join("identities")).unwrap();
        let attempts: Vec<_> = identities
            .lines()
            .map(|s| s.split_whitespace().collect::<Vec<_>>())
            .collect();
        assert_eq!(attempts.len(), 2);
        assert_eq!(
            attempts[0][..2],
            attempts[1][..2],
            "fallback retains the tab's assigned identity"
        );
        assert_ne!(attempts[0][2], attempts[1][2]);
        assert_eq!(records[0].pid.to_string(), attempts[1][2]);
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

#[test]
fn resume_fallback_is_once_only_and_preserves_other_errors_custom_args_and_signals() {
    use amux::{identity::Provider, session::resume_arguments};
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    for provider in [Provider::Codex, Provider::Claude] {
        let resume = resume_arguments(provider);
        for (args, first, fresh, status, calls) in [
            (resume, "1", "7", 7, 2),
            (resume, "1", "1", 1, 2),
            (resume, "0", "0", 0, 1),
            (resume, "2", "0", 2, 1),
            (resume, "signal", "0", 1, 1),
            (&[][..], "1", "1", 1, 1),
            (&["custom-resume-id"][..], "1", "0", 1, 1),
        ] {
            let fixture = tempfile::tempdir_in(&target).unwrap();
            let root = fixture.path();
            let output = Command::new(env!("CARGO_BIN_EXE_amux"))
                .args(["agent", provider.command(), "--"])
                .args(args)
                .current_dir(root)
                .env("PATH", resume_fixture(root, provider))
                .env("AMUX_STATE_DIR", root.join("state"))
                .env("AMUX_ARGS_FILE", root.join("args"))
                .env("AMUX_IDENTITY_FILE", root.join("identities"))
                .env("AMUX_RESUME_EXIT", first)
                .env("AMUX_FRESH_EXIT", fresh)
                .output()
                .unwrap();
            assert_eq!(
                output.status.code(),
                Some(status),
                "{provider:?} {first}/{fresh}"
            );
            let log = fs::read_to_string(root.join("args")).unwrap();
            assert_eq!(
                log.lines().count(),
                calls,
                "{provider:?} {first}/{fresh}: {log}"
            );
            if calls == 2 {
                assert!(log.ends_with("\n0:\n"));
            }
        }
    }
}
