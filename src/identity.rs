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
    /// Deterministic local project UUID; nil for legacy records.
    #[serde(default)]
    pub project_id: Uuid,
    /// Readable terminal agent name such as `codex-1`; empty for legacy records.
    #[serde(default)]
    pub agent_id: String,
    #[serde(default)]
    pub name: String,
}

impl AgentIdentity {
    /// Resolve the assignment supplied by amux, without OS ancestry discovery.
    pub fn assigned(selector: Option<&str>, project: &Path) -> Result<Self> {
        if let Some(selector) = selector {
            let directory = crate::project_identity::resolve(project)?.directory;
            let state = state_directory()?;
            let mut matching = Vec::new();
            for entry in std::fs::read_dir(state.join("agents"))
                .context("no amux agent assignments available")?
            {
                let path = entry?.path();
                if path.extension().is_none_or(|ext| ext != "json") {
                    continue;
                }
                let Ok(record) = serde_json::from_slice::<AgentRecord>(&std::fs::read(path)?)
                else {
                    continue;
                };
                let actor = record.identity;
                if actor.project_directory == directory
                    && (selector == actor.session_id.to_string()
                        || selector == actor.agent_id
                        || selector == actor.display_name())
                {
                    matching.push(actor);
                }
            }
            if matching.len() != 1 {
                bail!(
                    "agent assignment is missing or ambiguous; use the --agent ID supplied by amux"
                );
            }
            return Ok(matching.remove(0));
        }
        Self::from_environment()
            .context("use amux --agent <assigned-id> from this session's gateway notification")
    }

    pub fn display_name(&self) -> String {
        let person = if self.name.is_empty() {
            self.agent_id
                .rsplit_once('-')
                .and_then(|(_, number)| number.parse().ok())
                .map(assigned_name)
        } else {
            Some(self.name.clone())
        };
        let provider = match self.provider {
            Provider::Codex => "Codex",
            Provider::Claude => "Claude",
        };
        person
            .map(|name| format!("{name} - {provider}"))
            .unwrap_or_else(|| self.agent_id.clone())
    }

    /// Legacy machine dispatch verifies a live launch and caller ancestry.
    /// Ordinary task commands use the app-assigned identity instead.
    /// The UUID is amux's execution identity, not a provider-native conversation ID.
    /// Without a wrapper identity, a plain-shell native executor registered by the
    /// desktop is found in the caller's ancestry instead.
    pub fn verified_from_environment() -> Result<Self> {
        let directory = state_directory()?;
        let record = match Self::from_environment() {
            Ok(actor) => {
                let path = directory
                    .join("agents")
                    .join(format!("{}.json", actor.session_id));
                let record: AgentRecord = serde_json::from_slice(
                    &std::fs::read(path).context("agent launch is not registered")?,
                )?;
                if record.identity != actor {
                    bail!("calling identity does not match the registered execution");
                }
                record
            }
            Err(error) if environment_value("AMUX_AGENT_SESSION_ID").is_none() => {
                registered_ancestor(&directory)?.ok_or(error)?
            }
            Err(error) => return Err(error),
        };
        let actor = record.identity.clone();
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

    /// A nested launch under another agent always gets a fresh terminal number.
    pub fn new(provider: Provider, project: &Path) -> Result<Self> {
        let inherited = environment_value("AMUX_TERMINAL_NUMBER")
            .filter(|_| environment_value("AMUX_AGENT_SESSION_ID").is_none())
            .map(|value| {
                value
                    .parse::<u64>()
                    .ok()
                    .filter(|n| *n > 0)
                    .context("invalid AMUX_TERMINAL_NUMBER")
            })
            .transpose()?;
        let number = match inherited {
            Some(number) => number,
            None => next_terminal_number()?,
        };
        Self::for_terminal(provider, project, number)
    }

    /// The stable session for a provider in one project terminal, e.g. `codex-1`.
    /// Each launch still receives a fresh execution (instance) identity.
    pub fn for_terminal(provider: Provider, project: &Path, number: u64) -> Result<Self> {
        if number == 0 {
            bail!("terminal numbers start at 1");
        }
        let project = crate::project_identity::resolve(project)?;
        let agent_id = format!("{}-{number}", provider.command());
        Ok(Self {
            session_id: project.session_id(&agent_id),
            instance_id: Uuid::new_v4(),
            provider,
            os: std::env::consts::OS.to_owned(),
            project_directory: project.directory,
            project_id: project.id,
            agent_id,
            name: assigned_name(number),
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
            ("AMUX_AGENT_PROJECT_ID".into(), self.project_id.to_string()),
            ("AMUX_AGENT_ID".into(), self.agent_id.clone()),
            ("AMUX_AGENT_NAME".into(), self.name.clone()),
        ])
    }

    pub fn from_environment() -> Result<Self> {
        Self::from_values(
            &std::env::vars()
                .filter(|(k, v)| k.starts_with("AMUX_AGENT_") && !v.trim().is_empty())
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
        // Legacy launches predate the project UUID and readable agent name.
        let project_id = match values.get("AMUX_AGENT_PROJECT_ID") {
            Some(value) => value.parse().context("invalid project identity")?,
            None => Uuid::nil(),
        };
        Ok(Self {
            session_id,
            instance_id,
            provider,
            os,
            project_directory: PathBuf::from(get("AMUX_AGENT_PROJECT")?),
            project_id,
            agent_id: values.get("AMUX_AGENT_ID").cloned().unwrap_or_default(),
            name: values.get("AMUX_AGENT_NAME").cloned().unwrap_or_default(),
        })
    }
}

fn assigned_name(number: u64) -> String {
    let names: Vec<_> = include_str!("../assets/agent_names.txt").lines().collect();
    let index = number.saturating_sub(1);
    let name = names[(index % names.len() as u64) as usize];
    let cycle = index / names.len() as u64;
    if cycle == 0 {
        name.to_owned()
    } else {
        format!("{name} {}", cycle + 1)
    }
}

fn environment_value(name: &str) -> Option<String> {
    std::env::var(name)
        .ok()
        .filter(|value| !value.trim().is_empty())
}

/// The nearest live registered executor among the caller's ancestors.
fn registered_ancestor(directory: &Path) -> Result<Option<AgentRecord>> {
    let agents = live_agents(directory)?;
    let system = sysinfo::System::new_all();
    let mut pid = Some(sysinfo::Pid::from_u32(std::process::id()));
    for _ in 0..128 {
        let Some(current) = pid else { break };
        let mut matching = agents.iter().filter(|a| a.pid == current.as_u32());
        if let Some(record) = matching.next() {
            if matching.next().is_some() {
                bail!("several registered agents claim the calling executor");
            }
            return Ok(Some(record.clone()));
        }
        pid = system.process(current).and_then(sysinfo::Process::parent);
    }
    Ok(None)
}

/// Allocate the next global terminal number (starting at 1), persisted before use.
pub fn next_terminal_number() -> Result<u64> {
    next_terminal_number_at(&state_directory()?)
}

/// Reserve a number in an explicitly selected app state directory.
pub fn next_terminal_number_at(directory: &Path) -> Result<u64> {
    use std::io::{Read, Seek, Write};
    std::fs::create_dir_all(directory)?;
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(directory.join("terminal-number"))?;
    file.lock()?;
    let mut text = String::new();
    file.read_to_string(&mut text)?;
    let previous: u64 = if text.trim().is_empty() {
        0
    } else {
        text.trim()
            .parse()
            .context("invalid terminal number counter")?
    };
    let next = previous
        .checked_add(1)
        .context("terminal numbers exhausted")?;
    file.set_len(0)?;
    file.rewind()?;
    file.write_all(next.to_string().as_bytes())?;
    file.sync_all()?;
    Ok(next)
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
    /// Publish this record. Stable session IDs reuse a path, so an existing record
    /// is replaced only with positive proof that its process generation is gone.
    /// Re-registering the identical record is a no-op.
    pub fn register(&self) -> Result<()> {
        self.register_at(&state_directory()?)
    }

    /// `register` within an explicit state directory.
    pub fn register_at(&self, state: &Path) -> Result<()> {
        use std::io::Write;
        let root = state.join("agents");
        std::fs::create_dir_all(&root)?;
        let lock = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(false)
            .open(root.join(".lock"))?;
        lock.lock()?;
        let path = root.join(format!("{}.json", self.identity.session_id));
        match std::fs::read(&path) {
            Ok(bytes) => {
                let existing = serde_json::from_slice::<AgentRecord>(&bytes)
                    .context("invalid existing agent registration")?;
                if existing.identity == self.identity
                    && existing.pid == self.pid
                    && existing.process_start == self.process_start
                    && existing.process_generation == self.process_generation
                {
                    return Ok(());
                }
                if !process_is_gone(existing.pid, &existing.process_generation) {
                    bail!(
                        "agent {} is already running in this project; its live execution is not replaced",
                        existing.identity.agent_id
                    );
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        let mut file = tempfile::NamedTempFile::new_in(&root)?;
        file.write_all(&serde_json::to_vec(self)?)?;
        file.as_file().sync_all()?;
        file.persist(&path).map_err(|error| error.error)?;
        Ok(())
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
