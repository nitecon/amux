//! Pure fixture identities must not advance the installed app terminal counter.
use amux::identity::{AgentIdentity, Provider};
use std::{
    path::Path,
    sync::atomic::{AtomicU64, Ordering},
};
pub fn identity(provider: Provider, project: &Path) -> anyhow::Result<AgentIdentity> {
    static NUMBER: AtomicU64 = AtomicU64::new(1);
    AgentIdentity::for_terminal(provider, project, NUMBER.fetch_add(1, Ordering::Relaxed))
}
