//! Positive native identity of the outer application in an owned terminal tree.
//! This never inspects terminal output, input queues or provider prompt readiness.
use crate::identity::{self, Provider};
use serde::{Deserialize, Serialize};
use std::{
    collections::HashMap,
    path::{Path, PathBuf},
};
use sysinfo::{Pid, ProcessRefreshKind, ProcessesToUpdate, System, UpdateKind};

/// Bounds on one owned terminal tree; larger or deeper trees defer.
const MAX_TREE: usize = 1024;
const MAX_DEPTH: usize = 64;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct AgentProcess {
    pub pid: u32,
    pub generation: String,
    pub provider: Provider,
    pub directory: Option<PathBuf>,
}

/// Find the single verified Claude/Codex application that owns the terminal rooted at
/// `root_pid`. Linux requires the terminal's native foreground group; Windows passes
/// `None` and selects within the owned ConPTY tree. Shells, console hosts and the amux
/// wrapper are transparent; tool children of an application never replace it. Any
/// unknown, non-provider or ambiguous outer application defers with `None`.
pub fn discover(
    root_pid: u32,
    root_generation: &str,
    foreground_group: Option<i32>,
) -> Option<AgentProcess> {
    if root_pid == 0 || root_generation.is_empty() {
        return None;
    }
    #[cfg(target_os = "linux")]
    let group = Some(foreground_group.filter(|group| *group > 0)?);
    #[cfg(not(target_os = "linux"))]
    let group = {
        let _ = foreground_group;
        None
    };
    if !generation_is(root_pid, root_generation) {
        return None;
    }
    let first = select(root_pid, group)?;
    let fences = first
        .chain
        .iter()
        .map(|pid| Some((*pid, identity::process_generation(*pid).ok()?)))
        .collect::<Option<Vec<_>>>()?;
    // An independent second snapshot must agree, and no process on the owned ancestry
    // (root included) may have been replaced in between.
    let second = select(root_pid, group)?;
    if second != first
        || !generation_is(root_pid, root_generation)
        || fences
            .iter()
            .any(|(pid, fence)| !generation_is(*pid, fence))
    {
        return None;
    }
    Some(AgentProcess {
        pid: first.pid,
        generation: fences.first()?.1.clone(),
        provider: first.provider,
        directory: first.directory,
    })
}

/// Recheck that the same root generation still owns exactly the same application.
pub fn verify(
    root_pid: u32,
    root_generation: &str,
    foreground_group: Option<i32>,
    expected: &AgentProcess,
) -> bool {
    discover(root_pid, root_generation, foreground_group).as_ref() == Some(expected)
}

#[derive(Debug, PartialEq, Eq)]
struct Selection {
    pid: u32,
    provider: Provider,
    directory: Option<PathBuf>,
    /// Target first, then each owned ancestor up to and including the root.
    chain: Vec<u32>,
}

struct Node {
    parent: Option<u32>,
    image: Option<PathBuf>,
    script: Option<String>,
    directory: Option<PathBuf>,
}

fn select(root: u32, group: Option<i32>) -> Option<Selection> {
    let nodes = snapshot();
    nodes.get(&root)?;
    let mut outer = Vec::new();
    let mut size = 0usize;
    for (&pid, node) in &nodes {
        let Some(chain) = ancestry(&nodes, pid, root) else {
            continue;
        };
        size += 1;
        if size > MAX_TREE {
            return None;
        }
        if transparent(node) {
            continue;
        }
        // Only an application whose owned ancestors are all shells/hosts/wrappers is
        // the terminal's outer application; any other application above it wins.
        if chain[1..]
            .iter()
            .any(|ancestor| !transparent(&nodes[ancestor]))
        {
            continue;
        }
        if let Some(group) = group
            && !in_group(pid, group)?
        {
            continue;
        }
        outer.push((pid, chain));
    }
    // Siblings (background jobs, pipelines, parallel launches) are ambiguous.
    let [(pid, chain)] = <[_; 1]>::try_from(outer).ok()?;
    let node = &nodes[&pid];
    Some(Selection {
        pid,
        provider: provider(node)?,
        directory: node.directory.clone(),
        chain,
    })
}

/// Owned ancestry from `pid` up to `root`, or `None` when `pid` is outside the tree.
/// Every link must be a live parent that started no later than its child, so a
/// reused parent PID cannot adopt an unrelated process.
fn ancestry(nodes: &HashMap<u32, Node>, pid: u32, root: u32) -> Option<Vec<u32>> {
    let mut chain = vec![pid];
    let mut current = pid;
    while current != root {
        if chain.len() > MAX_DEPTH {
            return None;
        }
        let parent = nodes.get(&current)?.parent?;
        nodes.get(&parent)?;
        chain.push(parent);
        current = parent;
    }
    for link in chain.windows(2) {
        if start_ticks(link[1])? > start_ticks(link[0])? {
            return None;
        }
    }
    Some(chain)
}

/// Bounded native snapshot: executable, argv and cwd only. Environment is never read.
fn snapshot() -> HashMap<u32, Node> {
    let mut system = System::new();
    system.refresh_processes_specifics(
        ProcessesToUpdate::All,
        true,
        ProcessRefreshKind::nothing()
            .without_tasks()
            .with_exe(UpdateKind::Always)
            .with_cmd(UpdateKind::Always)
            .with_cwd(UpdateKind::Always),
    );
    system
        .processes()
        .iter()
        .filter(|(_, process)| {
            process.thread_kind().is_none()
                && !matches!(
                    process.status(),
                    sysinfo::ProcessStatus::Zombie | sysinfo::ProcessStatus::Dead
                )
        })
        .map(|(pid, process)| {
            (
                pid.as_u32(),
                Node {
                    parent: process.parent().map(Pid::as_u32),
                    image: process.exe().map(live_image),
                    script: process
                        .cmd()
                        .get(1)
                        .and_then(|arg| arg.to_str())
                        .filter(|arg| arg.len() <= 4096)
                        .map(|arg| arg.replace('\\', "/")),
                    directory: process.cwd().map(|path| {
                        crate::workspace_launch::external_path(
                            path.canonicalize().unwrap_or_else(|_| path.to_path_buf()),
                        )
                    }),
                },
            )
        })
        .collect()
}

/// Procfs marks a still-running executable whose file an updater replaced.
fn live_image(path: &Path) -> PathBuf {
    match path.file_name().and_then(|name| name.to_str()) {
        Some(name) if name.ends_with(" (deleted)") => {
            path.with_file_name(name.trim_end_matches(" (deleted)"))
        }
        _ => path.to_path_buf(),
    }
}

fn image_name(node: &Node) -> Option<String> {
    let name = node
        .image
        .as_ref()?
        .file_name()?
        .to_str()?
        .to_ascii_lowercase();
    Some(name.strip_suffix(".exe").unwrap_or(&name).to_owned())
}

/// Shells, console hosts and the amux wrapper never own provider input themselves.
/// An unreadable image is unknown, never transparent.
fn transparent(node: &Node) -> bool {
    image_name(node).is_some_and(|name| {
        matches!(
            name.as_str(),
            "bash"
                | "zsh"
                | "fish"
                | "sh"
                | "dash"
                | "ksh"
                | "csh"
                | "tcsh"
                | "cmd"
                | "powershell"
                | "pwsh"
                | "conhost"
                | "openconsole"
                | "amux"
        )
    })
}

/// Native provider images and their official Node entry points; never arbitrary argv.
fn provider(node: &Node) -> Option<Provider> {
    let image = node.image.as_ref()?;
    if let Some(provider) = identity::native_provider(image) {
        return Some(provider);
    }
    if !matches!(image_name(node)?.as_str(), "node" | "nodejs") {
        return None;
    }
    let script = node.script.as_deref()?;
    if script.ends_with("/@anthropic-ai/claude-code/cli.js") {
        Some(Provider::Claude)
    } else if script.ends_with("/@openai/codex/bin/codex.js") {
        Some(Provider::Codex)
    } else {
        None
    }
}

fn generation_is(pid: u32, expected: &str) -> bool {
    identity::process_generation(pid).is_ok_and(|generation| generation == expected)
}

/// Native creation ticks from the platform generation fence.
fn start_ticks(pid: u32) -> Option<u64> {
    identity::process_generation(pid)
        .ok()?
        .rsplit(':')
        .next()?
        .parse()
        .ok()
}

/// Whether a live process belongs to the given POSIX process group.
#[cfg(target_os = "linux")]
fn in_group(pid: u32, group: i32) -> Option<bool> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let fields: Vec<&str> = stat.rsplit_once(')')?.1.split_whitespace().collect();
    if matches!(*fields.first()?, "Z" | "X" | "x") {
        return Some(false);
    }
    Some(fields.get(2)?.parse::<i32>().ok()? == group)
}

#[cfg(not(target_os = "linux"))]
fn in_group(_pid: u32, _group: i32) -> Option<bool> {
    None
}
