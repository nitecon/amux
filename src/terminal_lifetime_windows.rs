//! Native Windows terminal ownership, independent of provider or project identity.
use anyhow::{Context, Result, bail};
use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
use windows_sys::Win32::{
    Foundation::{FILETIME, HANDLE, WAIT_TIMEOUT},
    System::{
        JobObjects::{
            AssignProcessToJobObject, CreateJobObjectW, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
            JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JobObjectExtendedLimitInformation,
            SetInformationJobObject, TerminateJobObject,
        },
        Threading::{
            GetProcessTimes, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION, PROCESS_SET_QUOTA,
            PROCESS_SYNCHRONIZE, PROCESS_TERMINATE, WaitForSingleObject,
        },
    },
};

/// Retains an anonymous, non-inherited kernel job for one directly spawned PTY
/// child. Children created after enrollment inherit its ownership. Existing
/// descendants from before enrollment are not retrospectively captured.
pub struct TerminalLifetime {
    job: OwnedHandle,
    closed: bool,
}

impl TerminalLifetime {
    /// Call immediately after native PTY creation, using only its initial child PID.
    /// Refuses nonchildren; project names and inherited agent UUIDs confer no authority.
    pub fn capture(pty_child_pid: u32) -> Result<Self> {
        let expected = crate::identity::process_generation(pty_child_pid)?;
        // SAFETY: a new owned handle, not inherited by future processes. Its native
        // identity is checked before any job assignment; later PID reuse is irrelevant.
        let raw = unsafe {
            OpenProcess(
                PROCESS_QUERY_LIMITED_INFORMATION
                    | PROCESS_SET_QUOTA
                    | PROCESS_TERMINATE
                    | PROCESS_SYNCHRONIZE,
                0,
                pty_child_pid,
            )
        };
        let process = owned(raw).context("open initial native PTY child")?;
        let mut creation = FILETIME::default();
        let mut exit = FILETIME::default();
        let mut kernel = FILETIME::default();
        let mut user = FILETIME::default();
        // SAFETY: retained process handle and initialized output parameters.
        check(unsafe {
            GetProcessTimes(
                process.as_raw_handle(),
                &mut creation,
                &mut exit,
                &mut kernel,
                &mut user,
            )
        })?;
        let generation = format!(
            "windows:{}",
            ((creation.dwHighDateTime as u64) << 32) | creation.dwLowDateTime as u64
        );
        let pid = sysinfo::Pid::from_u32(pty_child_pid);
        let mut system = sysinfo::System::new();
        system.refresh_processes_specifics(
            sysinfo::ProcessesToUpdate::Some(&[pid]),
            true,
            sysinfo::ProcessRefreshKind::nothing(),
        );
        if generation != expected
            || system
                .process(pid)
                .and_then(|process| process.parent())
                .is_none_or(|parent| parent.as_u32() != std::process::id())
            // SAFETY: a zero-timeout query of the retained native handle.
            || unsafe { WaitForSingleObject(process.as_raw_handle(), 0) } != WAIT_TIMEOUT
        {
            bail!("initial PTY process changed or is not a live direct child");
        }
        // SAFETY: null attributes create a non-inherited handle; null name makes
        // the job anonymous so another terminal cannot open it by project/name.
        let job = owned(unsafe { CreateJobObjectW(std::ptr::null(), std::ptr::null()) })
            .context("create terminal ownership job")?;
        let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
        limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        // SAFETY: correctly sized immutable structure, owned job handle. No
        // breakaway flags are enabled; all future descendants stay in this job.
        check(unsafe {
            SetInformationJobObject(
                job.as_raw_handle(),
                JobObjectExtendedLimitInformation,
                (&limits as *const JOBOBJECT_EXTENDED_LIMIT_INFORMATION).cast(),
                std::mem::size_of_val(&limits) as u32,
            )
        })
        .context("configure terminal ownership job")?;
        // SAFETY: assign only the retained, verified direct child, never amux or
        // a discovered peer. Incompatible enclosing jobs fail explicitly.
        check(unsafe { AssignProcessToJobObject(job.as_raw_handle(), process.as_raw_handle()) })
            .context("enroll initial PTY child in terminal ownership job")?;
        Ok(Self { job, closed: false })
    }

    /// Request termination of this job, even if its initial child already exited.
    /// Success is a kernel request result, not task cancellation or an outcome.
    pub fn retire(&mut self) -> Result<()> {
        if !self.closed {
            // SAFETY: the private retained job names only this terminal's enrolled
            // processes. No PID enumeration or retargeting occurs during cleanup.
            check(unsafe { TerminateJobObject(self.job.as_raw_handle(), 1) })
                .context("retire terminal ownership job")?;
            self.closed = true;
        }
        Ok(())
    }
}

impl Drop for TerminalLifetime {
    fn drop(&mut self) {
        let _ = self.retire();
        // Closing the final non-inherited job handle also requests termination,
        // including when amux exits without running destructors.
    }
}

fn check(result: i32) -> std::io::Result<()> {
    if result == 0 {
        Err(std::io::Error::last_os_error())
    } else {
        Ok(())
    }
}

fn owned(handle: HANDLE) -> std::io::Result<OwnedHandle> {
    if handle.is_null() {
        Err(std::io::Error::last_os_error())
    } else {
        // SAFETY: callers pass a newly returned owned handle exactly once.
        Ok(unsafe { OwnedHandle::from_raw_handle(handle) })
    }
}
