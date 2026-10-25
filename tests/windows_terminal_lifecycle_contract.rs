#![cfg(all(feature = "desktop", windows))]
use amux::{identity, terminal_lifetime::TerminalLifetime};
use iced::futures::{StreamExt, stream::BoxStream};
use iced_term::{Terminal, settings};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    io::Write,
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
#[ignore = "private native Windows activity fixture"]
fn windows_activity_fixture() {
    let Some(directory) = std::env::var_os("AMUX_WINDOWS_ACTIVITY_DIRECTORY") else {
        return;
    };
    let directory = Path::new(&directory);
    println!("ACTIVITY_READY");
    std::io::stdout().flush().unwrap();
    let deadline = Instant::now() + Duration::from_secs(60);
    let mut emitted = false;
    while !directory.join("stop").exists() {
        assert!(Instant::now() < deadline, "activity fixture timed out");
        if !emitted && directory.join("emit").exists() {
            println!("ACTIVITY_AFTER_IDLE");
            std::io::stdout().flush().unwrap();
            emitted = true;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
}

fn pump_activity(
    terminal: &mut Terminal,
    events: &mut BoxStream<'static, iced_term::Event>,
    runtime: &tokio::runtime::Runtime,
) {
    if let Ok(event) = runtime
        .block_on(async { tokio::time::timeout(Duration::from_millis(100), events.next()).await })
    {
        let iced_term::Event::BackendCall(id, command) = event.expect("activity stream closed");
        assert_eq!(id, terminal.id);
        terminal.handle(iced_term::Command::ProxyToBackend(command));
    }
}

#[test]
fn native_windows_redraws_settle_to_idle_and_new_output_reactivates() {
    let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&fixtures).unwrap();
    let state = tempfile::tempdir_in(fixtures).unwrap();
    let mut terminal = Terminal::new(
        980_451,
        settings::Settings {
            backend: settings::BackendSettings {
                program: std::env::current_exe().unwrap().display().to_string(),
                args: vec![
                    "--exact".into(),
                    "windows_activity_fixture".into(),
                    "--ignored".into(),
                    "--nocapture".into(),
                ],
                working_directory: Some(state.path().into()),
                env: [(
                    "AMUX_WINDOWS_ACTIVITY_DIRECTORY".into(),
                    state.path().display().to_string(),
                )]
                .into(),
            },
            ..Default::default()
        },
    )
    .unwrap();
    let _lifetime = TerminalLifetime::capture(terminal.child_pid().unwrap()).unwrap();
    let layout = iced::Size::new(800.0, 480.0);
    terminal.handle(iced_term::Command::ProxyToBackend(
        iced_term::BackendCommand::Resize(Some(layout), None),
    ));
    let mut events = iced::advanced::subscription::into_recipes(terminal.subscription())
        .pop()
        .unwrap()
        .stream(iced::futures::stream::empty().boxed());
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    while !terminal.screen_text().contains("ACTIVITY_READY") {
        assert!(Instant::now() < deadline, "activity fixture did not render");
        pump_activity(&mut terminal, &mut events, &runtime);
    }
    // Exercise the real ConPTY and the same font/layout sync path as redraws.
    // No screen contents or synthetic activity timestamp determine readiness.
    let deadline = Instant::now() + amux::session::IDLE_AFTER + Duration::from_secs(3);
    while Instant::now() < deadline {
        terminal.handle(iced_term::Command::ProxyToBackend(
            iced_term::BackendCommand::Resize(Some(layout), None),
        ));
        pump_activity(&mut terminal, &mut events, &runtime);
    }
    assert!(
        !amux::session::output_active(terminal.activity_elapsed()),
        "unchanged layout/redraws kept the Windows terminal active"
    );
    fs::write(state.path().join("emit"), "").unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    while !terminal.screen_text().contains("ACTIVITY_AFTER_IDLE") {
        assert!(Instant::now() < deadline, "new output did not render");
        pump_activity(&mut terminal, &mut events, &runtime);
    }
    assert!(amux::session::output_active(terminal.activity_elapsed()));
    fs::write(state.path().join("stop"), "").unwrap();
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
