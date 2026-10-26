//! A failed process query must not be confused with positive native death proof.
#![cfg(any(target_os = "linux", target_os = "macos", windows))]

use amux::identity::{process_generation, process_is_gone};
use std::process::{Child, Command, Stdio};

struct OwnedChild(Child);
impl Drop for OwnedChild {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

#[test]
#[ignore = "internal native child; returns immediately without its private fixture marker"]
fn process_proof_fixture() {
    if std::env::var_os("AMUX_PROCESS_PROOF_FIXTURE").as_deref() == Some(std::ffi::OsStr::new("1"))
    {
        std::thread::sleep(std::time::Duration::from_secs(20));
    }
}

#[test]
fn native_presence_generation_replacement_and_reaped_exit_are_distinct() {
    let mut child = OwnedChild(
        Command::new(std::env::current_exe().unwrap())
            .args(["--ignored", "--exact", "process_proof_fixture"])
            .env("AMUX_PROCESS_PROOF_FIXTURE", "1")
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap(),
    );
    let pid = child.0.id();
    let generation = process_generation(pid).unwrap();
    assert!(!process_is_gone(pid, &generation));
    assert!(!process_is_gone(pid, "unverifiable generation"));
    assert!(!process_is_gone(0, &generation));

    // Same PID with a different valid native creation identity cannot match
    // the original generation, even though a process is currently present.
    let (prefix, ticks) = generation.rsplit_once(':').unwrap();
    let replaced = format!("{prefix}:{}", ticks.parse::<u64>().unwrap() + 1);
    assert!(process_is_gone(pid, &replaced));

    child.0.kill().unwrap();
    child.0.wait().unwrap();
    assert!(process_is_gone(pid, &generation));
    assert!(!process_is_gone(pid, "unverifiable generation"));
}
