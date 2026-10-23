//! Explicit local workspace creation. No startup commands are persisted or replayed.
use anyhow::{Context, Result, bail};
use std::path::{Path, PathBuf};

pub fn home_directory() -> Result<PathBuf> {
    directories::BaseDirs::new()
        .context("Cannot find your home directory")?
        .home_dir()
        .canonicalize()
        .context("Cannot open your home directory")
}

pub fn directory(value: &str) -> Result<PathBuf> {
    let path = Path::new(value)
        .canonicalize()
        .context("Choose an existing directory")?;
    anyhow::ensure!(path.is_dir(), "Choose a directory, not a file");
    Ok(path)
}

pub fn script_command(path: &Path) -> Result<(String, Vec<String>)> {
    let path = path
        .canonicalize()
        .context("Choose an existing initialization script")?;
    anyhow::ensure!(path.is_file(), "Initialization script must be a file");
    let path = path
        .to_str()
        .context("Script path must be valid Unicode")?
        .to_owned();
    #[cfg(not(windows))]
    {
        // Source in an interactive shell: cd, exports and SSH prompts work normally.
        // The path is a positional argument, never interpolated into shell code.
        Ok((
            "/bin/sh".into(),
            vec![
                "-i".into(),
                "-c".into(),
                ". \"$1\"; exec \"${SHELL:-/bin/sh}\" -i".into(),
                "amux-init".into(),
                path,
            ],
        ))
    }
    #[cfg(windows)]
    {
        let extension = Path::new(&path)
            .extension()
            .and_then(|s| s.to_str())
            .unwrap_or_default();
        if extension.eq_ignore_ascii_case("ps1") {
            Ok((
                "powershell.exe".into(),
                vec!["-NoLogo".into(), "-NoExit".into(), "-File".into(), path],
            ))
        } else if extension.eq_ignore_ascii_case("cmd") || extension.eq_ignore_ascii_case("bat") {
            anyhow::ensure!(
                !path.contains(['%', '!', '\r', '\n', '"']),
                "Use a script path without cmd expansion characters"
            );
            Ok(("cmd.exe".into(), vec!["/D".into(), "/K".into(), path]))
        } else {
            bail!("Choose a PowerShell (.ps1) or command (.cmd/.bat) script");
        }
    }
}

pub fn clone_name(repository: &str) -> Result<String> {
    let repository = repository.trim().trim_end_matches('/');
    anyhow::ensure!(
        !repository.is_empty()
            && !repository.starts_with('-')
            && !repository.contains(['\r', '\n']),
        "Enter a repository URL or Git path"
    );
    let name = repository
        .rsplit(['/', '\\', ':'])
        .next()
        .unwrap_or_default()
        .trim_end_matches(".git");
    anyhow::ensure!(
        !name.is_empty()
            && name != "."
            && name != ".."
            && !name.contains(['?', '#', '%', '"', '<', '>', '|', '*']),
        "Repository needs a usable folder name"
    );
    Ok(name.into())
}

pub async fn clone_repository(parent: PathBuf, repository: String) -> Result<PathBuf> {
    use std::{io::Read, process::Stdio};
    let parent = directory(parent.to_str().context("Directory must be valid Unicode")?)?;
    let destination = parent.join(clone_name(&repository)?);
    // Reserve a new folder atomically; never clone into an existing checkout.
    std::fs::create_dir(&destination).with_context(|| {
        format!(
            "Cannot create {}. Choose a parent without an existing folder of that name",
            destination.display()
        )
    })?;
    let mut log = tempfile::tempfile()?;
    let mut command = tokio::process::Command::new("git");
    command
        .args(["clone", "--", repository.trim()])
        .arg(&destination)
        .current_dir(&parent)
        .env("GIT_TERMINAL_PROMPT", "0")
        .stdin(Stdio::null())
        .stdout(log.try_clone()?)
        .stderr(log.try_clone()?)
        .kill_on_drop(true);
    #[cfg(windows)]
    command.creation_flags(0x08000000);
    let status = tokio::time::timeout(std::time::Duration::from_secs(300), command.status()).await;
    let failure = match status {
        Ok(Ok(status)) if status.success() => return Ok(destination),
        Ok(Ok(status)) => format!("Git clone failed ({status})"),
        Ok(Err(error)) => format!("Cannot run Git: {error}"),
        Err(_) => "Git clone timed out after five minutes".into(),
    };
    use std::io::{Seek, SeekFrom};
    log.seek(SeekFrom::Start(0))?;
    let mut output = String::new();
    log.take(8192).read_to_string(&mut output).ok();
    bail!(
        "{failure}. {}\n{output}\nPartial folder preserved at {}",
        "Check Git installation and repository access.",
        destination.display()
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn repository_names_and_scripts_are_arguments_not_shell_code() {
        assert_eq!(
            clone_name("git@example.org:team/project.git").unwrap(),
            "project"
        );
        assert_eq!(
            clone_name("https://example.org/team/my project.git").unwrap(),
            "my project"
        );
        for bad in [
            "",
            "--help",
            "https://example.org/..",
            "https://example.org/repo?bad",
        ] {
            assert!(clone_name(bad).is_err());
        }
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        let script = fixture.path().join(if cfg!(windows) {
            "init space.ps1"
        } else {
            "init ' space.sh"
        });
        std::fs::write(&script, "").unwrap();
        let (_, args) = script_command(&script).unwrap();
        assert_eq!(
            args.last().unwrap(),
            script.canonicalize().unwrap().to_str().unwrap()
        );
        assert!(directory(script.to_str().unwrap()).is_err());
    }
    #[test]
    fn selected_scripts_execute_and_leave_an_interactive_shell() {
        use std::io::Write;
        use std::process::{Command, Stdio};
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        let cases: &[(&str, &str)] = if cfg!(windows) {
            &[
                ("init space.ps1", "Write-Output SCRIPT_RAN"),
                ("init space.cmd", "@echo SCRIPT_RAN"),
            ]
        } else {
            &[(
                "init ' space.sh",
                "printf 'SCRIPT_RAN\\n'\nread reply\nprintf 'REPLY=%s\\n' \"$reply\"\n",
            )]
        };
        for (name, content) in cases {
            let script = fixture.path().join(name);
            std::fs::write(&script, content).unwrap();
            let (program, args) = script_command(&script).unwrap();
            let mut child = Command::new(program)
                .args(args)
                .current_dir(fixture.path())
                .stdin(Stdio::piped())
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .spawn()
                .unwrap();
            let input: &[u8] = if cfg!(windows) {
                b"echo SHELL_ALIVE\r\nexit\r\n"
            } else {
                b"hello\nprintf 'SHELL_ALIVE\\n'\nexit\n"
            };
            child.stdin.take().unwrap().write_all(input).unwrap();
            let output = child.wait_with_output().unwrap();
            let stdout = String::from_utf8_lossy(&output.stdout);
            assert!(
                output.status.success(),
                "{name}: {stdout} {}",
                String::from_utf8_lossy(&output.stderr)
            );
            assert!(stdout.contains("SCRIPT_RAN"), "{name}: {stdout}");
            assert!(stdout.contains("SHELL_ALIVE"), "{name}: {stdout}");
            if !cfg!(windows) {
                assert!(stdout.contains("REPLY=hello"), "{stdout}");
            }
        }
    }

    #[tokio::test]
    async fn clone_creates_checkout_and_refuses_existing_destination() {
        let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&root).unwrap();
        let fixture = tempfile::tempdir_in(root).unwrap();
        let origin = fixture.path().join("origin.git");
        assert!(
            std::process::Command::new("git")
                .args(["init", "--bare"])
                .arg(&origin)
                .status()
                .unwrap()
                .success()
        );
        let parent = fixture.path().join("clone parent");
        std::fs::create_dir(&parent).unwrap();
        let checkout = clone_repository(parent.clone(), origin.display().to_string())
            .await
            .unwrap();
        assert!(checkout.join(".git").is_dir());
        std::fs::write(checkout.join("keep"), "untouched").unwrap();
        assert!(
            clone_repository(parent, origin.display().to_string())
                .await
                .is_err()
        );
        assert_eq!(
            std::fs::read_to_string(checkout.join("keep")).unwrap(),
            "untouched"
        );
    }
}
