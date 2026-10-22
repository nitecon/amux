#![cfg(all(feature = "desktop", windows))]
use amux::{identity, terminal_lifetime::TerminalLifetime};
use iced_term::{Terminal, settings};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    os::windows::{
        io::{AsRawHandle, FromRawHandle, OwnedHandle},
        process::CommandExt,
    },
    path::Path,
    process::{Command, Stdio},
    time::{Duration, Instant},
};
use windows_sys::Win32::{
    Foundation::WAIT_TIMEOUT,
    System::Threading::{
        CREATE_NEW_PROCESS_GROUP, CREATE_NO_WINDOW, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION,
        PROCESS_SYNCHRONIZE, PROCESS_TERMINATE, TerminateProcess, WaitForSingleObject,
    },
};

#[derive(Serialize, Deserialize)]
struct Record {
    pid: u32,
    generation: String,
}

#[test]
#[ignore = "private native Windows fixture; exits without its fixture environment"]
fn windows_lifetime_fixture() {
    let Some(directory) = std::env::var_os("AMUX_WINDOWS_LIFETIME_DIRECTORY") else {
        return;
    };
    let directory = Path::new(&directory);
    let role = std::env::var("AMUX_WINDOWS_LIFETIME_ROLE").unwrap();
    let pid = std::process::id();
    let record = Record {
        pid,
        generation: identity::process_generation(pid).unwrap(),
    };
    fs::write(
        directory.join(format!("{role}.json")),
        serde_json::to_vec(&record).unwrap(),
    )
    .unwrap();
    // Finite fallback if the parent fails before it can retain cleanup handles.
    let deadline = Instant::now() + Duration::from_secs(30);
    if role == "root" {
        while !directory.join("start").exists() {
            assert!(Instant::now() < deadline, "parent did not enroll the root");
            std::thread::sleep(Duration::from_millis(10));
        }
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "windows_lifetime_fixture", "--ignored"])
            .env("AMUX_WINDOWS_LIFETIME_ROLE", "background")
            // Make survival independent of the pseudoconsole being closed.
            .creation_flags(CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        while Instant::now() < deadline {
            if directory.join("exit-root").exists() {
                // No Rust destructors or polite console event: the enrolled
                // descendant must remain owned after its original parent exits.
                std::process::exit(0);
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        let _ = child.kill();
        let _ = child.wait();
    } else {
        while Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(10));
        }
    }
}

struct FixtureProcess(OwnedHandle);
impl FixtureProcess {
    fn capture(record: &Record) -> Self {
        // SAFETY: a new non-inherited native handle retained through every assertion.
        let raw = unsafe {
            OpenProcess(
                PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_TERMINATE | PROCESS_SYNCHRONIZE,
                0,
                record.pid,
            )
        };
        assert!(!raw.is_null());
        let handle = unsafe { OwnedHandle::from_raw_handle(raw) };
        assert_eq!(
            identity::process_generation(record.pid).unwrap(),
            record.generation,
            "fixture changed before retaining cleanup ownership"
        );
        Self(handle)
    }
    fn live(&self) -> bool {
        unsafe { WaitForSingleObject(self.0.as_raw_handle(), 0) == WAIT_TIMEOUT }
    }
}
impl Drop for FixtureProcess {
    fn drop(&mut self) {
        // SAFETY: retained native handle cannot retarget a recycled numeric PID.
        unsafe { TerminateProcess(self.0.as_raw_handle(), 1) };
    }
}

fn record(directory: &Path, role: &str) -> Record {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if let Ok(bytes) = fs::read(directory.join(format!("{role}.json")))
            && let Ok(record) = serde_json::from_slice(&bytes)
        {
            return record;
        }
        assert!(Instant::now() < deadline, "{role} fixture did not start");
        std::thread::sleep(Duration::from_millis(10));
    }
}

fn terminal(directory: &Path, role: &str, id: u64) -> Terminal {
    Terminal::new(
        id,
        settings::Settings {
            backend: settings::BackendSettings {
                program: std::env::current_exe().unwrap().display().to_string(),
                args: vec![
                    "--exact".into(),
                    "windows_lifetime_fixture".into(),
                    "--ignored".into(),
                ],
                working_directory: Some(directory.into()),
                env: [
                    (
                        "AMUX_WINDOWS_LIFETIME_DIRECTORY".into(),
                        directory.display().to_string(),
                    ),
                    ("AMUX_WINDOWS_LIFETIME_ROLE".into(), role.into()),
                ]
                .into(),
            },
            ..Default::default()
        },
    )
    .unwrap()
}

#[test]
fn native_windows_job_retires_background_descendants_and_preserves_peer_terminals() {
    assert!(TerminalLifetime::capture(std::process::id()).is_err());
    for leader_exits in [false, true] {
        let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        fs::create_dir_all(&fixtures).unwrap();
        let state = tempfile::tempdir_in(fixtures).unwrap();
        let directory = state.path();
        let owned = terminal(directory, "root", 980_001);
        let mut lifetime = TerminalLifetime::capture(owned.child_pid().unwrap()).unwrap();
        let root_record = record(directory, "root");
        assert_eq!(owned.child_pid(), Some(root_record.pid));
        let root = FixtureProcess::capture(&root_record);
        let peer = terminal(directory, "peer", 980_002);
        let peer_process = FixtureProcess::capture(&record(directory, "peer"));
        fs::write(directory.join("start"), b"enrolled").unwrap();
        let background = FixtureProcess::capture(&record(directory, "background"));
        if leader_exits {
            fs::write(directory.join("exit-root"), b"exit").unwrap();
            let deadline = Instant::now() + Duration::from_secs(3);
            while root.live() {
                assert!(Instant::now() < deadline, "root did not exit");
                std::thread::sleep(Duration::from_millis(10));
            }
            assert!(background.live(), "fixture must outlive its initial parent");
        } else {
            let started = Instant::now();
            lifetime.retire().unwrap();
            lifetime.retire().unwrap();
            assert!(started.elapsed() < Duration::from_secs(1));
        }
        drop(lifetime); // Before ConPTY, also covers the already-exited root case.
        drop(owned);
        let deadline = Instant::now() + Duration::from_secs(3);
        while root.live() || background.live() {
            assert!(
                Instant::now() < deadline,
                "owned native process survived close"
            );
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(
            peer_process.live(),
            "another terminal must remain independent"
        );
        drop(peer);
    }
}
