//! Checkout identity is upstream plus canonical directory; gateway names stay human-readable.
use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
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
    let (status, output) = crate::git::observe(
        directory,
        &["rev-parse", "--show-toplevel"],
        "checkout root",
    )
    .ok()?;
    if !status.success() || output.len() > 4096 {
        return None;
    }
    let root = PathBuf::from(output.trim_end_matches(['\r', '\n']));
    root.is_absolute().then_some(root)
}
