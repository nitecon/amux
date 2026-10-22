//! Local recipient fences. These preconditions never establish prompt readiness.
use crate::identity::{self, AgentIdentity, AgentRecord, Provider};
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};

fn processes(pids: &[u32]) -> sysinfo::System {
    let pids: Vec<_> = pids.iter().copied().map(sysinfo::Pid::from_u32).collect();
    let mut system = sysinfo::System::new();
    system.refresh_processes_specifics(
        sysinfo::ProcessesToUpdate::Some(&pids),
        true,
        sysinfo::ProcessRefreshKind::nothing(),
    );
    system
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
struct ProcessFence {
    pid: u32,
    generation: String,
}

/// Persisted address, never permission to write. Only a live recipient can verify it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct RecipientAddress {
    terminal_id: u64,
    identity: AgentIdentity,
    launcher: ProcessFence,
    agent: ProcessFence,
}

impl RecipientAddress {
    pub fn terminal_id(&self) -> u64 {
        self.terminal_id
    }

    pub fn identity(&self) -> &AgentIdentity {
        &self.identity
    }

    pub(crate) fn validate(&self) -> Result<()> {
        if self.terminal_id == u64::MAX
            || self.identity.session_id.is_nil()
            || self.identity.instance_id.is_nil()
            || !self.identity.project_directory.is_absolute()
            || self.identity.os.is_empty()
            || self.identity.os.len() > 32
            || [&self.launcher, &self.agent].iter().any(|process| {
                process.pid == 0 || process.generation.is_empty() || process.generation.len() > 256
            })
        {
            bail!("invalid terminal recipient address");
        }
        Ok(())
    }
}

impl ProcessFence {
    fn capture(pid: u32) -> Result<Self> {
        Ok(Self {
            pid,
            generation: identity::process_generation(pid)?,
        })
    }

    fn is_live(&self, system: &sysinfo::System) -> bool {
        system
            .process(sysinfo::Pid::from_u32(self.pid))
            .is_some_and(|process| {
                !matches!(
                    process.status(),
                    sysinfo::ProcessStatus::Zombie | sysinfo::ProcessStatus::Dead
                )
            })
            && identity::process_generation(self.pid)
                .is_ok_and(|generation| generation == self.generation)
    }
}

/// Ephemeral, exact terminal launch binding. A bound recipient never changes.
pub struct TerminalRecipient {
    launcher: ProcessFence,
    provider: Provider,
    project: PathBuf,
    agent: Option<AgentRecord>,
    input_epoch: u64,
    consent_epoch: u64,
    consent: bool,
}

/// Invalidated by human input, consent changes, process exit or generation change.
/// This is not a readiness proof or a terminal-write permission by itself.
pub struct RecipientCheckpoint {
    identity: AgentIdentity,
    input_epoch: u64,
    consent_epoch: u64,
}

/// Native executable image identity observed with the registered agent owning input.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[cfg(feature = "desktop")]
pub(crate) struct NativeInputEvidence {
    image_device: u64,
    image_inode: u64,
}

impl TerminalRecipient {
    pub fn new(launcher_pid: u32, provider: Provider, project: &Path) -> Result<Self> {
        Ok(Self {
            launcher: ProcessFence::capture(launcher_pid)?,
            provider,
            project: project.canonicalize()?,
            agent: None,
            input_epoch: 0,
            consent_epoch: 0,
            consent: false,
        })
    }

    /// Bind only a live registered agent directly owned by this terminal's launcher.
    /// Failed/stale bindings are never retargeted to a later registration.
    pub fn resolve(&mut self, registry: &Path) -> Result<&AgentIdentity> {
        if self.agent.is_none() {
            let records = identity::live_agents(registry)?;
            let mut pids: Vec<_> = records.iter().map(|r| r.pid).collect();
            pids.push(self.launcher.pid);
            let system = processes(&pids);
            if !self.launcher.is_live(&system) {
                bail!("terminal launcher is no longer live");
            }
            let mut candidates = records.into_iter().filter(|record| {
                record.identity.provider == self.provider
                    && record.identity.project_directory == self.project
                    && system
                        .process(sysinfo::Pid::from_u32(record.pid))
                        .and_then(|p| p.parent())
                        .is_some_and(|pid| pid.as_u32() == self.launcher.pid)
            });
            let record = candidates
                .next()
                .context("agent registration is not ready")?;
            if candidates.next().is_some() {
                bail!("terminal has an ambiguous agent registration");
            }
            self.agent = Some(record);
        }
        if !self.verify() {
            bail!("bound terminal recipient is no longer live");
        }
        Ok(&self.agent.as_ref().unwrap().identity)
    }

    fn verify(&self) -> bool {
        let Some(agent) = &self.agent else {
            return false;
        };
        let system = processes(&[self.launcher.pid, agent.pid]);
        self.launcher.is_live(&system)
            && agent.is_live(&system)
            && system
                .process(sysinfo::Pid::from_u32(agent.pid))
                .and_then(|p| p.parent())
                .is_some_and(|pid| pid.as_u32() == self.launcher.pid)
            && self.launcher.is_live(&processes(&[self.launcher.pid]))
    }

    pub fn address(&self, terminal_id: u64) -> Option<RecipientAddress> {
        if !self.verify() {
            return None;
        }
        let agent = self.agent.as_ref()?;
        let address = RecipientAddress {
            terminal_id,
            identity: agent.identity.clone(),
            launcher: self.launcher.clone(),
            agent: ProcessFence {
                pid: agent.pid,
                generation: agent.process_generation.clone(),
            },
        };
        address.validate().ok()?;
        Some(address)
    }

    pub fn matches_address(&self, address: &RecipientAddress) -> bool {
        self.address(address.terminal_id).as_ref() == Some(address)
    }

    #[cfg(all(feature = "desktop", target_os = "linux"))]
    pub(crate) fn native_input_evidence(
        &self,
        terminal: &iced_term::Terminal,
    ) -> Option<NativeInputEvidence> {
        use std::os::{
            fd::AsRawFd,
            unix::fs::{MetadataExt, OpenOptionsExt},
        };
        if !self.verify() {
            return None;
        }
        let agent = self.agent.as_ref()?;
        let executable = PathBuf::from(format!("/proc/{}/exe", agent.pid));
        let image_path = std::fs::read_link(&executable).ok()?;
        if image_path.file_name()? != std::ffi::OsStr::new(self.provider.command()) {
            return None;
        }
        let image = std::fs::metadata(&executable).ok()?;
        let open_input = |pid| {
            std::fs::OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_NOCTTY | libc::O_NONBLOCK)
                .open(format!("/proc/{pid}/fd/0"))
                .ok()
        };
        let launcher_input = open_input(self.launcher.pid)?;
        let agent_input = open_input(agent.pid)?;
        let launcher_device = launcher_input.metadata().ok()?.rdev();
        if launcher_device == 0 || agent_input.metadata().ok()?.rdev() != launcher_device {
            return None;
        }
        // SAFETY: valid open descriptors and an initialized integer output buffer.
        let mut foreground: libc::pid_t = 0;
        if unsafe {
            libc::ioctl(
                terminal.native_terminal_fd().as_raw_fd(),
                libc::TIOCGPGRP,
                &mut foreground,
            )
        } != 0
        {
            return None;
        }
        // SAFETY: read-only queries for a live registered process and its group.
        let group = unsafe { libc::getpgid(agent.pid as libc::pid_t) };
        if group <= 0 || foreground != group || !self.verify() {
            return None;
        }
        Some(NativeInputEvidence {
            image_device: image.dev(),
            image_inode: image.ino(),
        })
    }

    #[cfg(all(feature = "desktop", not(target_os = "linux")))]
    pub(crate) fn native_input_evidence(
        &self,
        _terminal: &iced_term::Terminal,
    ) -> Option<NativeInputEvidence> {
        // Native ConPTY/macOS evidence is still required; do not infer it from a PID.
        None
    }

    pub fn note_human_input(&mut self) {
        if let Some(next) = self.input_epoch.checked_add(1) {
            self.input_epoch = next;
        } else {
            // Saturation cannot let an ancient checkpoint become current again.
            self.consent = false;
        }
    }

    pub fn set_consent(&mut self, enabled: bool) {
        if let Some(next) = self.consent_epoch.checked_add(1) {
            self.consent_epoch = next;
            self.consent = enabled;
        } else {
            self.consent = false;
        }
    }

    pub fn checkpoint(&self) -> Option<RecipientCheckpoint> {
        if !self.consent || !self.verify() {
            return None;
        }
        Some(RecipientCheckpoint {
            identity: self.agent.as_ref()?.identity.clone(),
            input_epoch: self.input_epoch,
            consent_epoch: self.consent_epoch,
        })
    }

    pub fn is_current(&self, checkpoint: &RecipientCheckpoint) -> bool {
        self.consent
            && self.input_epoch == checkpoint.input_epoch
            && self.consent_epoch == checkpoint.consent_epoch
            && self
                .agent
                .as_ref()
                .is_some_and(|agent| agent.identity == checkpoint.identity)
            && self.verify()
    }
}
