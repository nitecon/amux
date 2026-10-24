//! An execution is a particular agent launch, never a project or provider label.
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashMap,
    path::{Path, PathBuf},
};
use uuid::Uuid;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, clap::ValueEnum)]
#[serde(rename_all = "lowercase")]
pub enum Provider {
    Codex,
    Claude,
}

impl Provider {
    pub fn command(self) -> &'static str {
        match self {
            Self::Codex => "codex",
            Self::Claude => "claude",
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AgentIdentity {
    pub session_id: Uuid,
    pub instance_id: Uuid,
    pub provider: Provider,
    pub os: String,
    pub project_directory: PathBuf,
}

impl AgentIdentity {
    /// Mutation authority requires a live launch and verified caller ancestry.
    /// The UUID is amux's execution identity, not a provider-native conversation ID.
    pub fn verified_from_environment() -> Result<Self> {
        let actor = Self::from_environment()?;
        let path = state_directory()?
            .join("agents")
            .join(format!("{}.json", actor.session_id));
        let record: AgentRecord = serde_json::from_slice(
            &std::fs::read(path).context("agent launch is not registered")?,
        )?;
        if record.identity != actor {
            bail!("calling identity does not match the registered execution");
        }
        let system = sysinfo::System::new_all();
        if !record.is_live(&system) {
            bail!("registered agent execution is no longer live");
        }
        let mut pid = sysinfo::Pid::from_u32(std::process::id());
        let mut executors = 0;
        let mut reached = false;
        for _ in 0..128 {
            let process = system
                .process(pid)
                .context("caller ancestry is unavailable")?;
            if let Some(provider) = executor_provider(process) {
                if provider != actor.provider {
                    bail!("nested provider must be launched with its own amux identity");
                }
                executors += 1;
            }
            if pid.as_u32() == record.pid {
                reached = true;
                break;
            }
            pid = process
                .parent()
                .context("caller is unrelated to the registered execution")?;
        }
        if !reached || executors != 1 {
            bail!(
                "caller does not resolve to one registered agent executor; nested agents need `amux agent`"
            );
        }
        if !record.is_live(&sysinfo::System::new_all()) {
            bail!("agent process changed during caller verification");
        }
        Ok(actor)
    }
    pub fn new(provider: Provider, project: &Path) -> Result<Self> {
        Ok(Self {
            session_id: Uuid::new_v4(),
            instance_id: Uuid::new_v4(),
            provider,
            os: std::env::consts::OS.to_owned(),
            project_directory: project
                .canonicalize()
                .context("resolve project directory")?,
        })
    }

    pub fn environment(&self) -> HashMap<String, String> {
        HashMap::from([
            ("AMUX_AGENT_SESSION_ID".into(), self.session_id.to_string()),
            (
                "AMUX_AGENT_INSTANCE_ID".into(),
                self.instance_id.to_string(),
            ),
            ("AMUX_AGENT_PROVIDER".into(), self.provider.command().into()),
            ("AMUX_AGENT_OS".into(), self.os.clone()),
            (
                "AMUX_AGENT_PROJECT".into(),
                self.project_directory.to_string_lossy().into(),
            ),
        ])
    }

    pub fn from_environment() -> Result<Self> {
        Self::from_values(
            &std::env::vars()
                .filter(|(k, _)| k.starts_with("AMUX_AGENT_"))
                .collect(),
        )
    }

    pub fn from_values(values: &HashMap<String, String>) -> Result<Self> {
        let get = |name: &str| {
            values.get(name).with_context(|| format!("missing {name}; launch this agent with `amux agent codex` or `amux agent claude`"))
        };
        let provider = match get("AMUX_AGENT_PROVIDER")?.as_str() {
            "codex" => Provider::Codex,
            "claude" => Provider::Claude,
            _ => bail!("unsupported AMUX_AGENT_PROVIDER"),
        };
        let os = get("AMUX_AGENT_OS")?.clone();
        if !matches!(os.as_str(), "linux" | "windows" | "macos") {
            bail!("unsupported agent OS");
        }
        let session_id: Uuid = get("AMUX_AGENT_SESSION_ID")?
            .parse()
            .context("invalid session identity")?;
        let instance_id: Uuid = get("AMUX_AGENT_INSTANCE_ID")?
            .parse()
            .context("invalid execution identity")?;
        if session_id.is_nil() || instance_id.is_nil() {
            bail!("agent identity cannot be nil");
        }
        Ok(Self {
            session_id,
            instance_id,
            provider,
            os,
            project_directory: PathBuf::from(get("AMUX_AGENT_PROJECT")?),
        })
    }
}

/// Known native executable layouts. A numeric filename alone is not a provider.
pub(crate) fn native_provider(path: &Path) -> Option<Provider> {
    let name = path.file_name()?.to_str()?.to_ascii_lowercase();
    let name = name.strip_suffix(".exe").unwrap_or(&name);
    match name {
        "codex" => return Some(Provider::Codex),
        "claude" => return Some(Provider::Claude),
        _ => {}
    }
    // The native Claude installer resolves its public `claude` symlink to
    // .../claude/versions/2.1.308. Inspect the real image, not argv[0], which
    // ordinary subprocesses can choose independently of their executable.
    let versions = path.parent()?;
    let parts: Vec<_> = name.split('.').collect();
    (versions.file_name()?.eq_ignore_ascii_case("versions")
        && versions
            .parent()?
            .file_name()?
            .eq_ignore_ascii_case("claude")
        && parts.len() == 3
        && parts
            .iter()
            .all(|part| !part.is_empty() && part.bytes().all(|byte| byte.is_ascii_digit())))
    .then_some(Provider::Claude)
}

pub(crate) fn executor_provider(process: &sysinfo::Process) -> Option<Provider> {
    let basename = |s: &std::ffi::OsStr| {
        Path::new(s)
            .file_name()
            .unwrap_or(s)
            .to_string_lossy()
            .to_ascii_lowercase()
    };
    let image = process.exe().unwrap_or_else(|| Path::new(process.name()));
    if let Some(provider) = native_provider(image) {
        return Some(provider);
    }
    let executable = basename(image.as_os_str());
    match executable.trim_end_matches(".exe") {
        "node" | "nodejs" => {
            // Codex's npm program is a launch shim; the Rust child is the executor.
            // Claude's Node CLI itself is the executor. Inspect known module paths,
            // never arbitrary prompt arguments containing a provider name.
            process.cmd().iter().skip(1).find_map(|arg| {
                let path = arg.to_string_lossy().replace('\\', "/");
                path.ends_with("/@anthropic-ai/claude-code/cli.js")
                    .then_some(Provider::Claude)
            })
        }
        "sh" | "bash" | "dash" => {
            process
                .cmd()
                .get(1)
                .and_then(|script| match basename(script).as_str() {
                    "codex" => Some(Provider::Codex),
                    "claude" => Some(Provider::Claude),
                    _ => None,
                })
        }
        _ => None,
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AgentRecord {
    pub identity: AgentIdentity,
    pub pid: u32,
    pub process_start: u64,
    pub process_generation: String,
}

impl AgentRecord {
    pub fn inspect(identity: AgentIdentity, pid: u32) -> Result<Self> {
        let system = sysinfo::System::new_all();
        let process = system
            .process(sysinfo::Pid::from_u32(pid))
            .context("agent process exited before registration")?;
        Ok(Self {
            identity,
            pid,
            process_start: process.start_time(),
            process_generation: process_generation(pid)?,
        })
    }
    pub fn is_live(&self, system: &sysinfo::System) -> bool {
        system
            .process(sysinfo::Pid::from_u32(self.pid))
            .is_some_and(|p| {
                p.start_time() == self.process_start
                    && !matches!(
                        p.status(),
                        sysinfo::ProcessStatus::Zombie | sysinfo::ProcessStatus::Dead
                    )
            })
            && process_generation(self.pid)
                .is_ok_and(|generation| generation == self.process_generation)
    }
}

/// A PID alone (or a start time rounded to seconds) is not a generation fence.
#[cfg(target_os = "linux")]
pub fn process_generation(pid: u32) -> Result<String> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat"))?;
    let end = stat.rfind(')').context("invalid process stat")?;
    let ticks = stat[end + 1..]
        .split_whitespace()
        .nth(19)
        .context("missing process start ticks")?;
    let boot = std::fs::read_to_string("/proc/sys/kernel/random/boot_id")?;
    Ok(format!("linux:{}:{ticks}", boot.trim()))
}

#[cfg(windows)]
pub fn process_generation(pid: u32) -> Result<String> {
    use windows_sys::Win32::{
        Foundation::{CloseHandle, FILETIME},
        System::Threading::{GetProcessTimes, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION},
    };
    // SAFETY: use a query-only handle, initialized out parameters and close exactly once.
    unsafe {
        let handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
        if handle.is_null() {
            return Err(std::io::Error::last_os_error().into());
        }
        let mut creation: FILETIME = std::mem::zeroed();
        let mut exit: FILETIME = std::mem::zeroed();
        let mut kernel: FILETIME = std::mem::zeroed();
        let mut user: FILETIME = std::mem::zeroed();
        let ok = GetProcessTimes(handle, &mut creation, &mut exit, &mut kernel, &mut user);
        let error = std::io::Error::last_os_error();
        CloseHandle(handle);
        if ok == 0 {
            return Err(error.into());
        }
        Ok(format!(
            "windows:{}",
            ((creation.dwHighDateTime as u64) << 32) | creation.dwLowDateTime as u64
        ))
    }
}

#[cfg(not(any(target_os = "linux", windows)))]
pub fn process_generation(_pid: u32) -> Result<String> {
    bail!("native process-generation verification is not implemented on this platform yet")
}

/// Positive evidence that an exact native process generation cannot run again.
/// Inability to inspect a process is not evidence that it exited.
pub fn process_is_gone(pid: u32, generation: &str) -> bool {
    if pid == 0 || !native_generation_format(generation) {
        return false;
    }
    match process_generation(pid) {
        Ok(current) => current != generation || process_is_exited(pid),
        Err(_) => process_is_absent(pid),
    }
}

#[cfg(windows)]
fn process_is_exited(pid: u32) -> bool {
    use windows_sys::Win32::{
        Foundation::{CloseHandle, WAIT_OBJECT_0},
        System::Threading::{OpenProcess, PROCESS_SYNCHRONIZE, WaitForSingleObject},
    };
    // A terminated Windows process may retain its PID and creation time while
    // another handle exists. A signaled process handle is positive exit proof.
    unsafe {
        let process = OpenProcess(PROCESS_SYNCHRONIZE, 0, pid);
        if process.is_null() {
            return false;
        }
        let exited = WaitForSingleObject(process, 0) == WAIT_OBJECT_0;
        CloseHandle(process);
        exited
    }
}

#[cfg(not(windows))]
fn process_is_exited(_pid: u32) -> bool {
    false
}

fn native_generation_format(generation: &str) -> bool {
    #[cfg(target_os = "linux")]
    {
        let Some((boot, ticks)) = generation
            .strip_prefix("linux:")
            .and_then(|value| value.split_once(':'))
        else {
            return false;
        };
        Uuid::parse_str(boot).is_ok_and(|id| !id.is_nil() && id.to_string() == boot)
            && ticks
                .parse::<u64>()
                .is_ok_and(|n| n > 0 && n.to_string() == ticks)
    }
    #[cfg(windows)]
    {
        generation.strip_prefix("windows:").is_some_and(|ticks| {
            ticks
                .parse::<u64>()
                .is_ok_and(|n| n > 0 && n.to_string() == ticks)
        })
    }
    #[cfg(not(any(target_os = "linux", windows)))]
    {
        let _ = generation;
        false
    }
}

#[cfg(target_os = "linux")]
fn process_is_absent(pid: u32) -> bool {
    let Ok(pid) = libc::pid_t::try_from(pid) else {
        return false;
    };
    // Signal zero only probes existence. EPERM, including hidden /proc entries,
    // stays unknown; only ESRCH proves that this PID is absent.
    (unsafe { libc::kill(pid, 0) == -1 })
        && std::io::Error::last_os_error().raw_os_error() == Some(libc::ESRCH)
}

#[cfg(windows)]
fn process_is_absent(pid: u32) -> bool {
    use windows_sys::Win32::{
        Foundation::{CloseHandle, ERROR_NO_MORE_FILES, GetLastError, INVALID_HANDLE_VALUE},
        System::Diagnostics::ToolHelp::{
            CreateToolhelp32Snapshot, PROCESSENTRY32W, Process32FirstW, Process32NextW,
            TH32CS_SNAPPROCESS,
        },
    };
    // A query-handle failure may mean access denied. Require a complete native
    // process snapshot instead, and close its non-inheritable handle once.
    unsafe {
        let snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
        if snapshot == INVALID_HANDLE_VALUE {
            return false;
        }
        let mut entry: PROCESSENTRY32W = std::mem::zeroed();
        entry.dwSize = std::mem::size_of::<PROCESSENTRY32W>() as u32;
        let mut next = Process32FirstW(snapshot, &mut entry);
        let mut seen = false;
        while next != 0 {
            seen = true;
            if entry.th32ProcessID == pid {
                CloseHandle(snapshot);
                return false;
            }
            next = Process32NextW(snapshot, &mut entry);
        }
        let complete = seen && GetLastError() == ERROR_NO_MORE_FILES;
        CloseHandle(snapshot);
        complete
    }
}

#[cfg(not(any(target_os = "linux", windows)))]
fn process_is_absent(_pid: u32) -> bool {
    false
}

pub fn state_directory() -> Result<PathBuf> {
    if let Some(path) = std::env::var_os("AMUX_STATE_DIR") {
        return Ok(path.into());
    }
    let dirs = directories::ProjectDirs::from("io", "amux", "amux")
        .context("no application data directory")?;
    Ok(dirs.data_local_dir().into())
}

pub fn live_agents(directory: &Path) -> Result<Vec<AgentRecord>> {
    let entries = match std::fs::read_dir(directory.join("agents")) {
        Ok(e) => e,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(e.into()),
    };
    let system = sysinfo::System::new_all();
    let mut agents = Vec::new();
    for entry in entries {
        let path = entry?.path();
        if path.extension().is_none_or(|ext| ext != "json") {
            continue;
        }
        let record: AgentRecord = match std::fs::read(&path)
            .ok()
            .and_then(|b| serde_json::from_slice(&b).ok())
        {
            Some(r) => r,
            None => continue,
        };
        if record.is_live(&system) {
            agents.push(record);
        }
    }
    agents.sort_by_key(|a| a.identity.session_id);
    Ok(agents)
}
