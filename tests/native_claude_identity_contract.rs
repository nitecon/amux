//! Versioned native installer layout, tested without an installed provider or network.
#![cfg(target_os = "linux")]
use amux::identity::{self, AgentIdentity};
use std::{
    fs,
    io::Write,
    os::unix::fs::symlink,
    path::Path,
    process::{Command, Stdio},
    time::{Duration, Instant},
};

#[test]
#[ignore = "internal native executable fixture; returns without private fixture environment"]
fn native_claude_fixture() {
    let Some(directory) = std::env::var_os("AMUX_NATIVE_CLAUDE_FIXTURE") else {
        return;
    };
    let directory = Path::new(&directory);
    if std::env::var_os("AMUX_NATIVE_CLAUDE_NESTED").is_some() {
        let result = AgentIdentity::verified_from_environment().map_err(|error| error.to_string());
        fs::write(
            directory.join("nested.json"),
            serde_json::to_vec(&result).unwrap(),
        )
        .unwrap();
        return;
    }
    let mut ready = String::new();
    std::io::stdin().read_line(&mut ready).unwrap();
    let result = AgentIdentity::verified_from_environment().map_err(|error| error.to_string());
    fs::write(
        directory.join("direct.json"),
        serde_json::to_vec(&result).unwrap(),
    )
    .unwrap();
    let nested = Command::new(std::env::current_exe().unwrap())
        .args(["--exact", "native_claude_fixture", "--ignored"])
        .env("AMUX_NATIVE_CLAUDE_NESTED", "1")
        .stdin(Stdio::null())
        .output()
        .unwrap();
    assert!(
        nested.status.success(),
        "fixture failed: {}",
        String::from_utf8_lossy(&nested.stderr)
    );
}

#[test]
fn versioned_native_claude_is_verified_but_unwrapped_nested_claude_is_rejected() {
    for known_layout in [true, false] {
        let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        fs::create_dir_all(&fixtures).unwrap();
        let state = tempfile::tempdir_in(fixtures).unwrap();
        let directory = state.path();
        let versions = directory.join(if known_layout {
            "claude/versions"
        } else {
            "unrelated/versions"
        });
        fs::create_dir_all(&versions).unwrap();
        let binary = versions.join("2.1.308");
        fs::copy(std::env::current_exe().unwrap(), &binary).unwrap();
        let bin = directory.join("bin");
        fs::create_dir_all(&bin).unwrap();
        symlink(&binary, bin.join("claude")).unwrap();
        let path = std::env::join_paths(std::iter::once(bin).chain(std::env::split_paths(
            &std::env::var_os("PATH").unwrap_or_default(),
        )))
        .unwrap();
        let mut child = Command::new(env!("CARGO_BIN_EXE_amux"))
            .args([
                "agent",
                "claude",
                "--",
                "--exact",
                "native_claude_fixture",
                "--ignored",
            ])
            .current_dir(directory)
            .env("PATH", path)
            .env("AMUX_STATE_DIR", directory.join("state"))
            .env("AMUX_NATIVE_CLAUDE_FIXTURE", directory)
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        let record = loop {
            if let Some(record) = identity::live_agents(&directory.join("state"))
                .unwrap()
                .pop()
            {
                break record;
            }
            if Instant::now() >= deadline {
                let _ = child.kill();
                let _ = child.wait();
                panic!("fixture failed to register");
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        child.stdin.as_mut().unwrap().write_all(b"ready\n").unwrap();
        let output = child.wait_with_output().unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        let direct: Result<AgentIdentity, String> =
            serde_json::from_slice(&fs::read(directory.join("direct.json")).unwrap()).unwrap();
        let nested: Result<AgentIdentity, String> =
            serde_json::from_slice(&fs::read(directory.join("nested.json")).unwrap()).unwrap();
        if known_layout {
            assert_eq!(
                direct.unwrap(),
                record.identity,
                "versioned native executable must retain its own registered identity"
            );
        } else {
            assert!(
                direct.is_err(),
                "a numeric filename in an unrelated layout is not a Claude executor"
            );
        }
        assert!(
            nested.unwrap_err().contains("nested agents need"),
            "nested native executable must not inherit parent authority"
        );
    }
}
