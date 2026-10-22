#![cfg(target_os = "linux")]
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::Path,
    process::{Command, Stdio},
    time::{Duration, Instant},
};

#[test]
fn abrupt_launcher_death_retires_hup_ignoring_direct_agent() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let root = fixture.path();
    fs::write(
        root.join("codex"),
        "#!/bin/sh\ntrap '' HUP TERM\nread -r stop\n",
    )
    .unwrap();
    fs::set_permissions(root.join("codex"), fs::Permissions::from_mode(0o700)).unwrap();
    let path = std::env::join_paths(std::iter::once(root.to_path_buf()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .unwrap();
    let mut launcher = Command::new(env!("CARGO_BIN_EXE_amux"))
        .args(["agent", "codex"])
        .current_dir(root)
        .env("PATH", path)
        .env("AMUX_STATE_DIR", root.join("state"))
        .stdin(Stdio::piped())
        .stderr(Stdio::null())
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
            let _ = launcher.kill();
            let _ = launcher.wait();
            panic!("agent did not register");
        }
        std::thread::sleep(Duration::from_millis(25));
    };
    launcher.kill().unwrap();
    launcher.wait().unwrap();
    let deadline = Instant::now() + Duration::from_secs(3);
    while record.is_live(&sysinfo::System::new_all()) {
        assert!(
            Instant::now() < deadline,
            "direct agent survived abrupt launcher death"
        );
        std::thread::sleep(Duration::from_millis(25));
    }
    assert!(
        amux::identity::live_agents(&root.join("state"))
            .unwrap()
            .is_empty(),
        "stale registry must not become live ownership"
    );
}
