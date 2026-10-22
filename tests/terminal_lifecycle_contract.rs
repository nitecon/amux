#![cfg(all(feature = "desktop", target_os = "linux"))]
use amux::identity::{self, AgentRecord};
use iced_term::{Terminal, settings};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    os::{
        fd::{AsRawFd, FromRawFd, OwnedFd},
        unix::fs::PermissionsExt,
    },
    path::Path,
    time::{Duration, Instant},
};

#[derive(Serialize, Deserialize)]
struct Job {
    pid: u32,
    generation: String,
    session: u32,
}
impl Job {
    fn live(&self) -> bool {
        let Ok(stat) = fs::read_to_string(format!("/proc/{}/stat", self.pid)) else {
            return false;
        };
        !matches!(
            stat.rsplit_once(')').unwrap().1.split_whitespace().next(),
            Some("Z" | "X")
        ) && identity::process_generation(self.pid)
            .is_ok_and(|generation| generation == self.generation)
    }
}

// Retain a native handle so failing assertions cannot leak fixtures or signal a reused PID.
struct FixtureCleanup(OwnedFd);
impl FixtureCleanup {
    fn new(pid: u32) -> Self {
        let fd = unsafe { libc::syscall(libc::SYS_pidfd_open, pid, 0) };
        assert!(fd >= 0);
        Self(unsafe { OwnedFd::from_raw_fd(fd as i32) })
    }
}
impl Drop for FixtureCleanup {
    fn drop(&mut self) {
        unsafe {
            libc::syscall(
                libc::SYS_pidfd_send_signal,
                self.0.as_raw_fd(),
                libc::SIGKILL,
                std::ptr::null::<libc::siginfo_t>(),
                0,
            );
        }
    }
}

#[test]
#[ignore = "internal native child fixture; exits immediately without its private fixture environment"]
fn background_fixture() {
    let Some(directory) = std::env::var_os("AMUX_BACKGROUND_DIRECTORY") else {
        return;
    };
    let kind = std::env::var("AMUX_BACKGROUND_KIND").unwrap();
    unsafe {
        if kind == "detached" {
            assert!(libc::setsid() > 0);
        } else {
            assert_eq!(libc::setpgid(0, 0), 0);
        }
        libc::signal(libc::SIGHUP, libc::SIG_IGN);
        libc::signal(libc::SIGTERM, libc::SIG_IGN);
    }
    let pid = std::process::id();
    let job = Job {
        pid,
        generation: identity::process_generation(pid).unwrap(),
        session: unsafe { libc::getsid(0) } as u32,
    };
    fs::write(
        Path::new(&directory).join(format!("{kind}.json")),
        serde_json::to_vec(&job).unwrap(),
    )
    .unwrap();
    loop {
        std::thread::park_timeout(Duration::from_secs(1));
    }
}

#[test]
fn closing_native_terminal_retires_owned_jobs_without_killing_detached_or_peer_sessions() {
    let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&fixtures).unwrap();
    let state = tempfile::tempdir_in(fixtures).unwrap();
    let directory = state.path();
    fs::write(directory.join("codex"), r#"#!/bin/sh
trap '' HUP TERM
AMUX_BACKGROUND_KIND=attached "$AMUX_BACKGROUND_EXECUTABLE" --exact background_fixture --ignored --nocapture &
AMUX_BACKGROUND_KIND=detached "$AMUX_BACKGROUND_EXECUTABLE" --exact background_fixture --ignored --nocapture &
read -r stop
"#).unwrap();
    fs::set_permissions(directory.join("codex"), fs::Permissions::from_mode(0o700)).unwrap();
    let path = std::env::join_paths(std::iter::once(directory.to_path_buf()).chain(
        std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default()),
    ))
    .unwrap();
    let terminal = Terminal::new(
        960_001,
        settings::Settings {
            backend: settings::BackendSettings {
                program: env!("CARGO_BIN_EXE_amux").into(),
                args: vec![
                    "--project-dir".into(),
                    directory.display().to_string(),
                    "agent".into(),
                    "codex".into(),
                ],
                working_directory: Some(directory.into()),
                env: [
                    ("PATH".into(), path.to_string_lossy().into()),
                    (
                        "AMUX_STATE_DIR".into(),
                        directory.join("state").display().to_string(),
                    ),
                    (
                        "AMUX_BACKGROUND_DIRECTORY".into(),
                        directory.display().to_string(),
                    ),
                    (
                        "AMUX_BACKGROUND_EXECUTABLE".into(),
                        std::env::current_exe().unwrap().display().to_string(),
                    ),
                ]
                .into(),
            },
            ..Default::default()
        },
    )
    .unwrap();
    let launcher_pid = terminal.child_pid().unwrap();
    let _root_cleanup = FixtureCleanup::new(launcher_pid);
    let mut lifetime = amux::terminal_lifetime::TerminalLifetime::capture(launcher_pid).unwrap();
    let peer = Terminal::new(
        960_002,
        settings::Settings {
            backend: settings::BackendSettings {
                program: "/bin/sh".into(),
                args: vec!["-c".into(), "read -r stop".into()],
                ..Default::default()
            },
            ..Default::default()
        },
    )
    .unwrap();
    let peer_pid = peer.child_pid().unwrap();
    let _peer_cleanup = FixtureCleanup::new(peer_pid);
    let deadline = Instant::now() + Duration::from_secs(10);
    let mut cleanup = Vec::new();
    let mut captured = std::collections::HashSet::new();
    let (attached, detached, agent): (Job, Job, AgentRecord) = loop {
        let attached: Option<Job> = fs::read(directory.join("attached.json"))
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok());
        let detached: Option<Job> = fs::read(directory.join("detached.json"))
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok());
        for job in [&attached, &detached].into_iter().flatten() {
            if captured.insert(job.pid) {
                cleanup.push(FixtureCleanup::new(job.pid));
            }
        }
        let agent = identity::live_agents(&directory.join("state"))
            .unwrap()
            .pop();
        if let (Some(attached), Some(detached), Some(agent)) = (attached, detached, agent) {
            break (attached, detached, agent);
        }
        assert!(Instant::now() < deadline, "background jobs did not start");
        std::thread::sleep(Duration::from_millis(20));
    };
    assert_eq!(attached.session, launcher_pid);
    assert_ne!(detached.session, launcher_pid);
    assert_ne!(
        unsafe { libc::getpgid(attached.pid as i32) } as u32,
        launcher_pid
    );
    let started = Instant::now();
    lifetime.retire().unwrap();
    assert!(
        started.elapsed() < Duration::from_millis(250),
        "cleanup request must remain bounded"
    );
    drop(terminal);
    let deadline = Instant::now() + Duration::from_secs(2);
    while attached.live() || agent.is_live(&sysinfo::System::new_all()) {
        assert!(
            Instant::now() < deadline,
            "owned background job survived native terminal close"
        );
        std::thread::sleep(Duration::from_millis(20));
    }
    assert!(
        detached.live(),
        "explicitly detached work is outside terminal session ownership"
    );
    assert!(
        identity::process_generation(peer_pid).is_ok(),
        "closing one terminal must preserve peer session"
    );
    drop(peer);
}
