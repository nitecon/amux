//! Only local layout is persisted. Neither task ownership nor agent execution is restored.
use crate::identity::Provider;
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashSet,
    io::{Read, Write},
    path::{Path, PathBuf},
};

#[derive(Clone, Serialize, Deserialize)]
pub struct SavedTab {
    pub id: u64,
    pub title: String,
    pub provider: Option<Provider>,
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Layout {
    Pane {
        tabs: Vec<SavedTab>,
        active: usize,
    },
    Split {
        vertical: bool,
        ratio: f32,
        a: Box<Layout>,
        b: Box<Layout>,
    },
}
#[derive(Clone, Serialize, Deserialize)]
pub struct SavedWorkspace {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub init_script: Option<PathBuf>,
    pub directory: PathBuf,
    pub layout: Layout,
}
#[derive(Clone, Serialize, Deserialize)]
pub struct Snapshot {
    pub version: u32,
    pub active: usize,
    pub workspaces: Vec<SavedWorkspace>,
}

impl Snapshot {
    fn validate(&self) -> Result<()> {
        if self.version != 1
            || self.workspaces.is_empty()
            || self.workspaces.len() > 64
            || self.active >= self.workspaces.len()
        {
            bail!("invalid session workspace state");
        }
        let mut ids = HashSet::new();
        fn walk(node: &Layout, ids: &mut HashSet<u64>, depth: usize) -> Result<()> {
            if depth > 16 {
                bail!("session split depth exceeds 16");
            }
            match node {
                Layout::Pane { tabs, active } => {
                    if tabs.is_empty() || *active >= tabs.len() {
                        bail!("invalid saved tab selection");
                    }
                    for tab in tabs {
                        if tab.id == u64::MAX || !ids.insert(tab.id) || ids.len() > 256 {
                            bail!("invalid or duplicate terminal identity");
                        }
                    }
                }
                Layout::Split { ratio, a, b, .. } => {
                    if !ratio.is_finite() || !(0.05..=0.95).contains(ratio) {
                        bail!("invalid saved split ratio");
                    }
                    walk(a, ids, depth + 1)?;
                    walk(b, ids, depth + 1)?;
                }
            }
            Ok(())
        }
        for workspace in &self.workspaces {
            if !workspace.directory.is_absolute()
                || workspace
                    .init_script
                    .as_ref()
                    .is_some_and(|path| !path.is_absolute())
            {
                bail!("saved workspace directory and script must be absolute");
            }
            walk(&workspace.layout, &mut ids, 0)?;
        }
        Ok(())
    }
    pub fn load(path: &Path) -> Result<Option<Self>> {
        let file = match std::fs::File::open(path) {
            Ok(f) => f,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(e) => return Err(e.into()),
        };
        let mut bytes = Vec::new();
        file.take(512 * 1024 + 1).read_to_end(&mut bytes)?;
        if bytes.len() > 512 * 1024 {
            bail!("session snapshot exceeds 512 KiB");
        }
        let snapshot: Self = serde_json::from_slice(&bytes)
            .context("invalid session snapshot; original file left intact")?;
        snapshot.validate()?;
        Ok(Some(snapshot))
    }
    pub fn save(&self, path: &Path) -> Result<()> {
        self.validate()?;
        let directory = path.parent().context("session path has no parent")?;
        std::fs::create_dir_all(directory)?;
        let bytes = serde_json::to_vec_pretty(self)?;
        if bytes.len() > 512 * 1024 {
            bail!("session snapshot exceeds 512 KiB");
        }
        let mut temporary = tempfile::NamedTempFile::new_in(directory)?;
        temporary.write_all(&bytes)?;
        temporary.as_file().sync_all()?;
        temporary.persist(path).map_err(|e| e.error)?;
        Ok(())
    }
}
