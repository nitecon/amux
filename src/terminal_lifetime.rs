//! Linux terminal-session ownership. Detached sessions are deliberately excluded.
//! Signals are termination requests, not a gateway outcome or a teardown acknowledgment.
use crate::identity::process_generation;
use anyhow::{Context, Result, bail};
use std::{
    fs,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    time::{Duration, Instant},
};

struct Process {
    pid: u32,
    generation: String,
    handle: OwnedFd,
}

impl Process {
    fn capture(pid: u32) -> Result<Self> {
        let generation = process_generation(pid)?;
        // SAFETY: pidfd_open returns a new owned descriptor; flags must be zero.
        let fd = unsafe { libc::syscall(libc::SYS_pidfd_open, pid, 0) };
        if fd < 0 {
            return Err(std::io::Error::last_os_error().into());
        }
        let process = Self {
            pid,
            generation,
            handle: unsafe { OwnedFd::from_raw_fd(fd as i32) },
        };
        if !process.current() {
            bail!("process changed while capturing native ownership");
        }
        Ok(process)
    }

    fn current(&self) -> bool {
        process_generation(self.pid).is_ok_and(|generation| generation == self.generation)
    }

    fn kill(&self) -> std::io::Result<()> {
        // SAFETY: the retained pidfd names the captured process, even after PID reuse.
        // A concurrent exit yields ESRCH; it can never signal a new process at that PID.
        let result = unsafe {
            libc::syscall(
                libc::SYS_pidfd_send_signal,
                self.handle.as_raw_fd(),
                libc::SIGKILL,
                std::ptr::null::<libc::siginfo_t>(),
                0,
            )
        };
        if result < 0 {
            let error = std::io::Error::last_os_error();
            if error.raw_os_error() != Some(libc::ESRCH) {
                return Err(error);
            }
        }
        Ok(())
    }
}

/// Capture only the direct native PTY child, immediately after creating that PTY.
/// Alacritty creates a new session with that child as leader. Never pass an agent
/// discovered merely by project/provider name or by an inherited UUID.
///
/// Dropping this guard before the terminal requests retirement of processes still
/// in its owned session, including different job-control process groups. Work
/// explicitly detached into another session is outside this boundary. An exited
/// leader cannot authorize a new cleanup scan; uncertain ownership fails closed.
pub struct TerminalLifetime {
    root: Process,
    closed: bool,
}

impl TerminalLifetime {
    pub fn capture(pty_child_pid: u32) -> Result<Self> {
        let root = Process::capture(pty_child_pid)?;
        if session(root.pid) != Some(root.pid) {
            bail!("PTY child is not its terminal session leader");
        }
        Ok(Self {
            root,
            closed: false,
        })
    }

    /// Bounded scan and native-handle signals. Does not wait on or reap arbitrary
    /// descendants; the existing PTY backend remains responsible for its direct child.
    pub fn retire(&mut self) -> Result<()> {
        if self.closed {
            return Ok(());
        }
        self.closed = true;
        if !self.root.current() || session(self.root.pid) != Some(self.root.pid) {
            return Ok(());
        }
        let deadline = Instant::now() + Duration::from_millis(100);
        let mut members = Vec::new();
        let mut complete = true;
        for entry in fs::read_dir("/proc").context("enumerate owned terminal session")? {
            if Instant::now() >= deadline || members.len() >= 256 {
                complete = false;
                break;
            }
            let Ok(entry) = entry else {
                continue;
            };
            let Some(pid) = entry
                .file_name()
                .to_str()
                .and_then(|name| name.parse::<u32>().ok())
            else {
                continue;
            };
            if pid != self.root.pid
                && session(pid) == Some(self.root.pid)
                && let Ok(process) = Process::capture(pid)
                && session(pid) == Some(self.root.pid)
            {
                members.push(process);
            }
        }
        // Keep the leader alive until ownership has been rechecked and all captured
        // jobs have received their requests. No numeric-PID or project-wide kill.
        if !self.root.current() || session(self.root.pid) != Some(self.root.pid) {
            return Ok(());
        }
        let mut failure = None;
        for process in members {
            if process.current()
                && session(process.pid) == Some(self.root.pid)
                && let Err(error) = process.kill()
            {
                failure = Some(error);
            }
        }
        if let Err(error) = self.root.kill() {
            failure = Some(error);
        }
        if let Some(error) = failure {
            return Err(error.into());
        }
        if !complete {
            bail!("terminal cleanup scan reached its bound; complete retirement unconfirmed");
        }
        Ok(())
    }
}

impl Drop for TerminalLifetime {
    fn drop(&mut self) {
        let _ = self.retire();
    }
}

fn session(pid: u32) -> Option<u32> {
    // SAFETY: read-only process query, with explicit error handling.
    let session = unsafe { libc::getsid(pid.try_into().ok()?) };
    (session >= 0).then_some(session as u32)
}
