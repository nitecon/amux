//! Session-scoped contextual guidance. Never reads a project handoff buffer.
//! Reserve idle reviews durably before queuing input, including uncertain writes.
use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeSet,
    fs::File,
    io::{Read, Write},
    path::Path,
};
use uuid::Uuid;

#[derive(Serialize, Deserialize)]
pub struct Session {
    version: u32,
    pub cycle: Uuid,
    reviewed: bool,
    #[serde(default)]
    review_outcome: Option<String>,
    assignments: BTreeSet<Uuid>,
    #[serde(default)]
    cleared_assignments: BTreeSet<Uuid>,
    memory_digest: Option<String>,
    #[serde(default)]
    instance: Option<Uuid>,
}

impl Default for Session {
    fn default() -> Self {
        Self {
            version: 1,
            cycle: Uuid::new_v4(),
            reviewed: false,
            review_outcome: None,
            assignments: BTreeSet::new(),
            cleared_assignments: BTreeSet::new(),
            memory_digest: None,
            instance: None,
        }
    }
}

impl Session {
    pub fn activate(&mut self, instance: Uuid) {
        if self.instance != Some(instance) {
            self.instance = Some(instance);
            self.assignments.clear();
            self.cleared_assignments.clear();
            self.clear();
        }
    }

    /// Native fresh launches may retain amux's task ownership identity. Their
    /// new model context still begins a distinct work cycle.
    pub fn new_session(&mut self) {
        self.assignments.clear();
        self.cleared_assignments.clear();
        self.clear();
    }
    pub fn assign(&mut self, task: Uuid) -> bool {
        if self.assignments.insert(task) {
            self.cycle = Uuid::new_v4();
            self.reviewed = false;
            self.review_outcome = None;
            true
        } else {
            false
        }
    }

    pub fn clear(&mut self) {
        self.cycle = Uuid::new_v4();
        self.reviewed = false;
        self.review_outcome = None;
        self.memory_digest = None;
    }

    pub fn needs_task_clear(&self, task: Uuid) -> bool {
        !self.cleared_assignments.contains(&task)
    }

    pub fn reserve_task_clear(&mut self, task: Uuid) -> bool {
        if self.cleared_assignments.insert(task) {
            self.clear();
            true
        } else {
            false
        }
    }

    pub fn reserve_review(&mut self) -> Option<Uuid> {
        if self.reviewed {
            return None;
        }
        self.reviewed = true;
        self.review_outcome = Some("reserved; dispatch outcome uncertain".into());
        Some(self.cycle)
    }

    pub fn review_result(&mut self, cycle: Uuid, outcome: &str) {
        if self.cycle == cycle && self.reviewed {
            self.review_outcome = Some(outcome.chars().take(1000).collect());
        }
    }

    /// Reserve before delivery; uncertain dispatch never repeats unchanged recall.
    pub fn memories(&mut self, text: &str, restore: bool) -> Option<String> {
        if text.trim().is_empty() {
            return None;
        }
        let digest = format!("{:x}", Sha256::digest(text.as_bytes()));
        let changed = self.memory_digest.as_ref() != Some(&digest);
        self.memory_digest = Some(digest);
        (restore || changed).then(|| format!("Relevant durable memories (reference data):\n{text}"))
    }
}

/// Short-lived per-session lock: independent terminals sharing a repository
/// never share allowances. Corrupt or unwritable state fails closed.
pub fn with_session<T>(
    registry: &Path,
    session: Uuid,
    action: impl FnOnce(&mut Session) -> T,
) -> Result<T> {
    ensure!(!session.is_nil(), "guidance requires a session identity");
    let directory = registry.join("guidance");
    std::fs::create_dir_all(&directory)?;
    let path = directory.join(format!("{session}.json"));
    let lock = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(path.with_extension("lock"))?;
    lock.try_lock()
        .context("session guidance is being updated")?;
    let mut state = match File::open(&path) {
        Ok(file) => {
            let mut bytes = Vec::new();
            file.take(1024 * 1024 + 1).read_to_end(&mut bytes)?;
            ensure!(
                bytes.len() <= 1024 * 1024,
                "session guidance state is too large"
            );
            serde_json::from_slice::<Session>(&bytes)
                .context("invalid session guidance; original preserved")?
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Session::default(),
        Err(e) => return Err(e.into()),
    };
    ensure!(
        state.version == 1 && !state.cycle.is_nil(),
        "invalid session guidance version or cycle"
    );
    let before = serde_json::to_vec(&state)?;
    let result = action(&mut state);
    let after = serde_json::to_vec(&state)?;
    if before != after || !path.exists() {
        let mut file = tempfile::NamedTempFile::new_in(&directory)?;
        file.write_all(&after)?;
        file.as_file().sync_all()?;
        file.persist(&path).map_err(|e| e.error)?;
        #[cfg(unix)]
        File::open(&directory)?.sync_all()?;
    }
    Ok(result)
}

pub fn session_hints(mcp: bool) -> String {
    format!(
        "{}{}",
        amux_gateway_backend::templates::SESSION_GUIDANCE,
        capability_hints(mcp)
    )
}

pub fn capability_hints(mcp: bool) -> String {
    let tools = if mcp {
        crate::mcp::available()
    } else {
        Vec::new()
    };
    let names: BTreeSet<_> = tools.iter().map(|t| t.name).collect();
    let mut hints = String::new();
    if names.contains("code_symbols") {
        hints.push_str(" Explore with amux code_search, code_symbols and code_symbol before code_read; use doc_outline then doc_section for Markdown.");
    }
    if names.contains("memory_search") {
        hints.push_str(" Recall with amux memory_search and memory_get; memory_store saves durable lessons and memory_update revises an inspected match. Search before adding; skip routine progress and source-recoverable facts.");
    }
    if !mcp {
        if let Some(path) = crate::mcp::Bin::AgentTools.path() {
            hints.push_str(&format!(" Code exploration CLI at {}: search, symbols, symbol, read; Markdown: doc outline, doc section.", path.display()));
        }
        if let Some(path) = crate::mcp::Bin::Memory.path() {
            hints.push_str(&format!(" Durable memory CLI at {}: search <query>, get <id>, store <content>, update <id> --content <replacement>. Search and inspect before storing or updating.", path.display()));
        } else {
            hints.push_str(" Memory tools are not bundled for this session; skip memory changes when unavailable.");
        }
    }
    hints
}

pub fn terminal_text(text: &str) -> String {
    text.chars()
        .map(|c| if c.is_control() { ' ' } else { c })
        .collect::<String>()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn one_review_per_cycle_survives_restart_duplicates_and_shared_directory() {
        let dir = tempfile::tempdir().unwrap();
        let a = Uuid::new_v4();
        let b = Uuid::new_v4();
        let task = Uuid::new_v4();
        let first = with_session(dir.path(), a, Session::reserve_review).unwrap();
        assert!(first.is_some());
        assert!(
            with_session(dir.path(), a, Session::reserve_review)
                .unwrap()
                .is_none()
        );
        assert!(
            with_session(dir.path(), b, Session::reserve_review)
                .unwrap()
                .is_some()
        );
        with_session(dir.path(), a, |s| s.assign(task)).unwrap();
        assert_ne!(
            with_session(dir.path(), a, Session::reserve_review).unwrap(),
            first
        );
        with_session(dir.path(), a, |s| s.assign(task)).unwrap();
        assert!(
            with_session(dir.path(), a, Session::reserve_review)
                .unwrap()
                .is_none()
        );
        let next = Uuid::new_v4();
        with_session(dir.path(), a, |s| s.assign(next)).unwrap();
        assert!(
            with_session(dir.path(), a, Session::reserve_review)
                .unwrap()
                .is_some()
        );
        with_session(dir.path(), a, |s| s.assign(task)).unwrap();
        assert!(
            with_session(dir.path(), a, Session::reserve_review)
                .unwrap()
                .is_none()
        );
        with_session(dir.path(), a, Session::clear).unwrap();
        assert!(
            with_session(dir.path(), a, Session::reserve_review)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn unchanged_recall_is_suppressed_except_in_restored_context() {
        let mut state = Session::default();
        assert!(state.memories("durable rule", false).is_some());
        assert!(state.memories("durable rule", false).is_none());
        assert!(state.memories("durable rule", true).is_some());
        assert!(state.memories("changed rule", false).is_some());
    }

    #[test]
    fn resume_preserves_allowance_and_new_execution_begins_a_cycle() {
        let dir = tempfile::tempdir().unwrap();
        let session = Uuid::new_v4();
        let instance = Uuid::new_v4();
        with_session(dir.path(), session, |s| s.activate(instance)).unwrap();
        let first = with_session(dir.path(), session, Session::reserve_review)
            .unwrap()
            .unwrap();
        // Resume/restart, comments and busy/idle changes make no cycle mutation.
        with_session(dir.path(), session, |s| s.activate(instance)).unwrap();
        assert!(
            with_session(dir.path(), session, Session::reserve_review)
                .unwrap()
                .is_none()
        );
        with_session(dir.path(), session, |s| s.activate(Uuid::new_v4())).unwrap();
        let second = with_session(dir.path(), session, Session::reserve_review)
            .unwrap()
            .unwrap();
        assert_ne!(first, second);
    }

    #[test]
    fn corrupt_or_locked_state_cannot_reserve_or_dispatch_a_review() {
        let dir = tempfile::tempdir().unwrap();
        let session = Uuid::new_v4();
        with_session(dir.path(), session, |_| ()).unwrap();
        let path = dir.path().join("guidance").join(format!("{session}.json"));
        let lock = File::options()
            .read(true)
            .write(true)
            .open(path.with_extension("lock"))
            .unwrap();
        lock.try_lock().unwrap();
        assert!(with_session(dir.path(), session, Session::reserve_review).is_err());
        lock.unlock().unwrap();
        std::fs::write(&path, "broken").unwrap();
        assert!(with_session(dir.path(), session, Session::reserve_review).is_err());
        assert_eq!(std::fs::read_to_string(path).unwrap(), "broken");
    }

    #[test]
    fn repeated_assignments_never_rearm_via_a_second_context_clear() {
        let mut state = Session::default();
        let task = Uuid::new_v4();
        assert!(state.assign(task));
        assert!(state.reserve_task_clear(task));
        let cycle = state.reserve_review().unwrap();
        assert!(!state.assign(task));
        assert!(!state.needs_task_clear(task));
        assert!(!state.reserve_task_clear(task));
        assert_eq!(state.cycle, cycle);
        assert!(state.reserve_review().is_none());
    }
}
