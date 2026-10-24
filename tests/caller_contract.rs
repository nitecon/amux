#![cfg(unix)]
use std::{
    fs,
    io::Write,
    os::unix::fs::PermissionsExt,
    path::Path,
    process::{Command, Stdio},
    time::{Duration, Instant},
};

#[test]
fn task_mutation_accepts_agent_tool_ancestry_but_rejects_nested_or_unrelated_callers() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    let operation = "amux tasks --project demo --gateway-url http://127.0.0.1:9 claim 00000000-0000-4000-8000-000000000001";
    let script = format!(
        "#!/bin/sh\nif [ \"$1\" = nested ]; then {operation}; result=$?; exit $result; fi\nread -r go\ncase $MODE in same) codex nested ;; other) claude ;; *) {operation} ;; esac\nresult=$?\nexit $result\n"
    );
    fs::write(root.join("codex"), script).unwrap();
    fs::write(
        root.join("claude"),
        format!("#!/bin/sh\n{operation}\nresult=$?\nexit $result\n"),
    )
    .unwrap();
    for file in ["codex", "claude"] {
        fs::set_permissions(root.join(file), fs::Permissions::from_mode(0o700)).unwrap();
    }
    let path = std::env::join_paths(std::iter::once(root.to_path_buf()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .unwrap();
    for (mode, expected) in [
        ("direct", "gateway unavailable"),
        ("same", "nested agents need"),
        ("other", "nested provider"),
    ] {
        let mut child = Command::new(env!("CARGO_BIN_EXE_amux"))
            .args(["agent", "codex"])
            .current_dir(root)
            .env("PATH", &path)
            .env("MODE", mode)
            .env("AMUX_GATEWAY_KEY", "test-only")
            .env("AMUX_STATE_DIR", root.join("state"))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        let record = loop {
            if let Some(record) = amux::identity::live_agents(&root.join("state"))
                .unwrap()
                .pop()
            {
                break record;
            }
            if Instant::now() > deadline {
                let _ = child.kill();
                let _ = child.wait();
                panic!("agent did not register");
            }
            std::thread::sleep(Duration::from_millis(25));
        };
        let unrelated = Command::new(env!("CARGO_BIN_EXE_amux"))
            .args([
                "tasks",
                "--project",
                "demo",
                "--gateway-url",
                "http://127.0.0.1:9",
                "claim",
                "00000000-0000-4000-8000-000000000001",
            ])
            .current_dir(root)
            .envs(record.identity.environment())
            .env("AMUX_STATE_DIR", root.join("state"))
            .env("AMUX_GATEWAY_KEY", "test-only")
            .output()
            .unwrap();
        assert!(!unrelated.status.success());
        // Run beneath a real Claude session (e.g. Claude Code), the caller's
        // ancestry meets that executor first and is refused as a nested provider
        // before the unrelated-ancestry check. Either way it is an authorization
        // refusal; a transport error must never count as rejection.
        let stderr = String::from_utf8_lossy(&unrelated.stderr);
        assert!(!stderr.contains("gateway unavailable"), "{stderr}");
        assert!(
            stderr.contains("unrelated")
                || stderr.contains("does not resolve")
                || stderr.contains("nested provider must be launched with its own amux identity"),
            "{stderr}"
        );
        child.stdin.as_mut().unwrap().write_all(b"go\n").unwrap();
        let output = child.wait_with_output().unwrap();
        assert!(!output.status.success());
        assert!(
            String::from_utf8_lossy(&output.stderr).contains(expected),
            "mode={mode} stderr={}",
            String::from_utf8_lossy(&output.stderr)
        );
    }
}

#[test]
fn plain_shell_agent_uses_desktop_registration_without_inherited_actor_fields() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    fs::write(root.join("codex"), "#!/bin/sh\nread -r go\n\"$AMUX_TEST_CLI\" tasks --project demo claim 00000000-0000-4000-8000-000000000001\n").unwrap();
    fs::set_permissions(root.join("codex"), fs::Permissions::from_mode(0o700)).unwrap();
    amux::gateway_preferences::GatewayPreferences {
        url: "http://127.0.0.1:9".into(),
        api_key: "test-only".into(),
        ..Default::default()
    }
    .save(&root.join("gateway-preferences.json"))
    .unwrap();
    let mut command = Command::new(root.join("codex"));
    for (key, _) in std::env::vars_os() {
        if key.to_string_lossy().starts_with("AMUX_AGENT_")
            || key.to_string_lossy().starts_with("AMUX_GATEWAY_")
        {
            command.env_remove(key);
        }
    }
    let mut child = command
        .current_dir(root)
        .env("AMUX_STATE_DIR", root)
        .env("AMUX_TEST_CLI", env!("CARGO_BIN_EXE_amux"))
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let actor =
        amux::identity::AgentIdentity::for_terminal(amux::identity::Provider::Codex, root, 1)
            .unwrap();
    amux::identity::AgentRecord::inspect(actor, child.id())
        .unwrap()
        .register_at(root)
        .unwrap();
    child.stdin.as_mut().unwrap().write_all(b"go\n").unwrap();
    let output = child.wait_with_output().unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    // Reaching transport proves saved URL/key and the actual registered executor
    // were accepted; no provider hook or inherited actor UUID was supplied.
    assert!(!output.status.success());
    assert!(stderr.contains("gateway unavailable"), "{stderr}");
    assert!(!stderr.contains("missing AMUX_AGENT"), "{stderr}");
}
