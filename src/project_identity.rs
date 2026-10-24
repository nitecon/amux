//! Checkout identity is upstream plus canonical directory; gateway names stay human-readable.
use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use std::{
    io::Read,
    path::{Path, PathBuf},
    process::{Command, Stdio},
    time::{Duration, Instant},
};
use uuid::Uuid;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ProjectIdentity {
    pub id: Uuid,
    pub directory: PathBuf,
    pub upstream: Option<String>,
    pub gateway_project: Option<String>,
}
impl ProjectIdentity {
    pub fn session_id(&self, agent_name: &str) -> Uuid {
        Uuid::new_v5(&self.id, agent_name.as_bytes())
    }
}

pub fn from_parts(directory: &Path, upstream: Option<&str>) -> Result<ProjectIdentity> {
    let directory = crate::workspace_launch::external_path(
        directory
            .canonicalize()
            .context("resolve checkout directory")?,
    );
    ensure!(directory.is_dir(), "project directory must be a directory");
    let upstream = upstream
        .map(|s| crate::gateway_project::repository(s).context("unsupported Git upstream"))
        .transpose()?;
    let gateway_project = upstream
        .as_deref()
        .map(|s| {
            let name = s
                .rsplit('/')
                .next()
                .unwrap_or_default()
                .to_ascii_lowercase();
            crate::gateway::validate_project(&name)?;
            Ok::<_, anyhow::Error>(name)
        })
        .transpose()?;
    let input = serde_json::to_vec(&serde_json::json!([
        "amux-project-v1",
        upstream.as_deref().unwrap_or_default(),
        directory.to_string_lossy()
    ]))?;
    Ok(ProjectIdentity {
        id: Uuid::new_v5(&Uuid::NAMESPACE_URL, &input),
        directory,
        upstream,
        gateway_project,
    })
}

pub fn resolve(directory: &Path) -> Result<ProjectIdentity> {
    let directory = crate::workspace_launch::external_path(
        directory
            .canonicalize()
            .context("resolve project directory")?,
    );
    let root = checkout_root(&directory).unwrap_or(directory);
    let upstream = crate::gateway_project::origin(&root).ok();
    from_parts(&root, upstream.as_deref())
}

fn checkout_root(directory: &Path) -> Option<PathBuf> {
    let mut command = Command::new("git");
    command
        .args([
            "--no-optional-locks",
            "-c",
            "core.hooksPath=/dev/null",
            "-C",
        ])
        .arg(directory)
        .args(["rev-parse", "--show-toplevel"])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    for (key, _) in std::env::vars_os() {
        if key.to_string_lossy().starts_with("GIT_") {
            command.env_remove(key);
        }
    }
    command.env("GIT_TERMINAL_PROMPT", "0");
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        command.creation_flags(windows_sys::Win32::System::Threading::CREATE_NO_WINDOW);
    }
    let mut child = command.spawn().ok()?;
    let deadline = Instant::now() + Duration::from_secs(2);
    let status = loop {
        if let Some(status) = child.try_wait().ok()? {
            break status;
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let _ = child.wait();
            return None;
        }
        std::thread::sleep(Duration::from_millis(10));
    };
    if !status.success() {
        return None;
    }
    let mut output = String::new();
    child
        .stdout
        .take()?
        .take(4097)
        .read_to_string(&mut output)
        .ok()?;
    if output.len() > 4096 {
        return None;
    }
    let root = PathBuf::from(output.trim_end_matches(['\r', '\n']));
    root.is_absolute().then_some(root)
}
