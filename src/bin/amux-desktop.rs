//! Native desktop entry point, without a console window on Windows. Agent tabs
//! run the sibling `amux` CLI, so agent tools keep their stdout and stderr.
#![cfg_attr(windows, windows_subsystem = "windows")]

#[path = "../desktop.rs"]
mod desktop;

use clap::Parser;
use std::{path::PathBuf, process::ExitCode};

/// Commands and agents use the `amux` CLI; this entry point only opens the desktop.
#[derive(Parser)]
#[command(name = "amux-desktop", version)]
struct Args {
    #[arg(long)]
    project_dir: Option<PathBuf>,
}

fn main() -> ExitCode {
    #[cfg(target_os = "macos")]
    adopt_login_path();
    match Args::parse()
        .project_dir
        .map(Ok)
        .unwrap_or_else(amux::workspace_launch::home_directory)
        .and_then(desktop::run)
    {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("amux-desktop: {error:#}");
            ExitCode::FAILURE
        }
    }
}

/// Finder and the Dock start apps with a minimal PATH. Agent tabs run claude or
/// codex directly, not through a shell, so adopt the login shell's PATH (with
/// any current entries after it) before any thread or child process starts.
#[cfg(target_os = "macos")]
fn adopt_login_path() {
    use std::{
        process::{Command, Stdio},
        time::{Duration, Instant},
    };
    const MARK: &str = "__AMUX_PATH__";
    let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/zsh".into());
    let Ok(mut child) = Command::new(shell)
        .args(["-ilc", &format!("printf '{MARK}%s{MARK}' \"$PATH\"")])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
    else {
        return;
    };
    // A slow or interactive shell profile must not block the desktop.
    let deadline = Instant::now() + Duration::from_secs(5);
    while child.try_wait().ok().flatten().is_none() {
        if Instant::now() > deadline {
            let _ = child.kill();
            let _ = child.wait();
            return;
        }
        std::thread::sleep(Duration::from_millis(20));
    }
    let Ok(output) = child.wait_with_output() else {
        return;
    };
    let output = String::from_utf8_lossy(&output.stdout);
    let Some(login) = output.split(MARK).nth(1).filter(|p| !p.is_empty()) else {
        return;
    };
    let mut entries: Vec<&str> = login.split(':').collect();
    let current = std::env::var("PATH").unwrap_or_default();
    for entry in current.split(':') {
        if !entry.is_empty() && !entries.contains(&entry) {
            entries.push(entry);
        }
    }
    // SAFETY: called first in main, before any other thread exists.
    unsafe { std::env::set_var("PATH", entries.join(":")) };
}
