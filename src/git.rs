//! Git process setup shared by bounded, read-only checkout observations and
//! work submission.
use anyhow::{Context, Result, bail};
use std::{
    io::Read,
    path::Path,
    process::{Command, ExitStatus, Stdio},
    thread,
    time::{Duration, Instant},
};

/// A `git` command that cannot prompt and ignores inherited `GIT_*` variables,
/// which could redirect it to another repository, index or work tree. `keep`
/// names inherited variables to preserve, such as push transport settings.
pub(crate) fn command(keep: &[&str]) -> Command {
    let mut command = Command::new("git");
    command.stdin(Stdio::null());
    for (key, _) in std::env::vars_os() {
        let name = key.to_string_lossy();
        if name.starts_with("GIT_") && !keep.contains(&name.as_ref()) {
            command.env_remove(&key);
        }
    }
    command.env("GIT_TERMINAL_PROMPT", "0");
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        command.creation_flags(windows_sys::Win32::System::Threading::CREATE_NO_WINDOW);
    }
    command
}

/// Keep the exit status so callers distinguish absent configuration from a
/// failed repository lookup. Errors never include arguments or Git output.
/// Return at most 4097 bytes: callers apply their 4096-byte limits either to
/// raw output or, for configuration values, after trimming Git's line ending.
pub(crate) fn observe(
    directory: &Path,
    arguments: &[&str],
    purpose: &'static str,
) -> Result<(ExitStatus, String)> {
    let mut command = command(&[]);
    command
        .args([
            "--no-optional-locks",
            "-c",
            "core.hooksPath=/dev/null",
            "-C",
        ])
        .arg(directory)
        .args(arguments)
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    let mut child = command
        .spawn()
        .with_context(|| format!("cannot run git to read the {purpose}"))?;
    let deadline = Instant::now() + Duration::from_secs(2);
    let status = (|| -> Result<ExitStatus> {
        loop {
            if let Some(status) = child
                .try_wait()
                .with_context(|| format!("cannot wait for git reading the {purpose}"))?
            {
                return Ok(status);
            }
            if Instant::now() >= deadline {
                bail!("git timed out reading the {purpose}");
            }
            thread::sleep(Duration::from_millis(10));
        }
    })();
    if status.is_err() {
        let _ = child.kill();
        let _ = child.wait();
    }
    let status = status?;
    let mut output = String::new();
    if let Some(stdout) = child.stdout.take() {
        stdout
            .take(4097)
            .read_to_string(&mut output)
            .with_context(|| format!("git returned an unreadable {purpose}"))?;
    }
    Ok((status, output))
}
