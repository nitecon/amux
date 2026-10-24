//! Native process fixtures are copies of this repo-built test binary under
//! target/test-fixtures, renamed to shell/provider/tool aliases. No installed
//! provider, model turn or gateway is ever started.
use amux::{
    delegation_process::{self, AgentProcess},
    identity::{self, Provider},
};
use std::{
    io::{BufRead, BufReader, Read, Write},
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    sync::OnceLock,
};

const MODE: &str = "AMUX_PROCESS_FIXTURE";
const LINE: &str = "AMUX_FIXTURE ";
const READY: &str = "AMUX_FIXTURE_READY";
const ALIASES: [&str; 5] = ["sh", "amux", "claude", "codex", "git"];

/// Fixture mode: `branch(+branch)*`, `branch = [%]name(>[%]name)*`. Each process
/// launches its branches, reports every pid, then lives until its stdin closes.
/// `%` starts the child in its own process group, as job-control shells do.
#[test]
#[ignore = "fixture process entry point, run only by the contract tests"]
fn fixture_process() {
    let Ok(spec) = std::env::var(MODE) else {
        return;
    };
    let exe = std::env::current_exe().unwrap();
    let name = exe.file_stem().unwrap().to_str().unwrap().to_owned();
    let mut lines = vec![format!("{LINE}{name} {}", std::process::id())];
    let mut children = Vec::new();
    for branch in spec.split('+').filter(|branch| !branch.is_empty()) {
        let (head, rest) = branch.split_once('>').unwrap_or((branch, ""));
        let (group, child) = head
            .strip_prefix('%')
            .map_or((false, head), |child| (true, child));
        let mut child = launch(exe.parent().unwrap(), child, rest, group);
        lines.extend(ready(&mut child));
        children.push(child);
    }
    let mut out = std::io::stdout().lock();
    for line in lines {
        writeln!(out, "{line}").unwrap();
    }
    writeln!(out, "{READY}").unwrap();
    out.flush().unwrap();
    drop(out);
    let _ = std::io::stdin().read_to_end(&mut Vec::new());
    for mut child in children {
        drop(child.stdin.take());
        let _ = child.wait();
    }
}

fn fixtures() -> &'static Path {
    static DIR: OnceLock<PathBuf> = OnceLock::new();
    DIR.get_or_init(|| {
        let exe = std::env::current_exe().unwrap();
        // target/<profile>/deps/<test> -> target/test-fixtures
        let parent = exe.ancestors().nth(3).unwrap().join("test-fixtures");
        std::fs::create_dir_all(&parent).unwrap();
        for stale in std::fs::read_dir(&parent).unwrap().flatten() {
            if stale
                .file_name()
                .to_string_lossy()
                .starts_with("delegation-process-")
            {
                let _ = std::fs::remove_dir_all(stale.path());
            }
        }
        let dir = parent.join(format!("delegation-process-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        for alias in ALIASES {
            std::fs::copy(&exe, dir.join(executable(alias))).unwrap();
        }
        std::fs::canonicalize(dir).unwrap()
    })
}

fn executable(alias: &str) -> String {
    format!("{alias}{}", std::env::consts::EXE_SUFFIX)
}

fn launch(dir: &Path, alias: &str, spec: &str, group: bool) -> Child {
    let mut command = Command::new(dir.join(executable(alias)));
    command
        .args([
            "--exact",
            "fixture_process",
            "--ignored",
            "--nocapture",
            "--test-threads=1",
            "-q",
        ])
        .env(MODE, spec)
        .current_dir(dir)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null());
    #[cfg(unix)]
    if group {
        use std::os::unix::process::CommandExt;
        command.process_group(0);
    }
    #[cfg(not(unix))]
    let _ = group;
    command.spawn().unwrap()
}

fn ready(child: &mut Child) -> Vec<String> {
    let mut lines = Vec::new();
    for line in BufReader::new(child.stdout.as_mut().unwrap()).lines() {
        let line = line.unwrap();
        if line == READY {
            return lines;
        }
        if line.starts_with(LINE) {
            lines.push(line);
        }
    }
    panic!("fixture exited before reporting ready");
}

struct Tree {
    root: Child,
    generation: String,
    pids: Vec<(String, u32)>,
}

impl Tree {
    fn spawn(root: &str, spec: &str) -> Self {
        let (group, root) = root
            .strip_prefix('%')
            .map_or((false, root), |root| (true, root));
        let mut child = launch(fixtures(), root, spec, group);
        let pids = ready(&mut child)
            .into_iter()
            .map(|line| {
                let (name, pid) = line[LINE.len()..].split_once(' ').unwrap();
                (name.to_owned(), pid.parse().unwrap())
            })
            .collect();
        let generation = identity::process_generation(child.id()).unwrap();
        Self {
            root: child,
            generation,
            pids,
        }
    }

    fn pid(&self, name: &str) -> u32 {
        self.pids.iter().find(|(n, _)| n == name).unwrap().1
    }

    fn discover(&self, foreground: Option<i32>) -> Option<AgentProcess> {
        delegation_process::discover(self.root.id(), &self.generation, foreground)
    }

    fn agent(&self, name: &str, provider: Provider) -> AgentProcess {
        let pid = self.pid(name);
        AgentProcess {
            pid,
            generation: identity::process_generation(pid).unwrap(),
            provider,
            directory: Some(amux::workspace_launch::external_path(
                fixtures().to_path_buf(),
            )),
        }
    }
}

impl Drop for Tree {
    fn drop(&mut self) {
        let _ = self.root.kill();
        let _ = self.root.wait();
    }
}

/// The terminal foreground group the root supplies: Linux TIOCGPGRP, Windows none.
fn foreground(pid: u32) -> Option<i32> {
    #[cfg(target_os = "linux")]
    {
        let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).unwrap();
        Some(
            stat.rsplit_once(')')
                .unwrap()
                .1
                .split_whitespace()
                .nth(2)
                .unwrap()
                .parse()
                .unwrap(),
        )
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = pid;
        None
    }
}

#[test]
fn outer_provider_behind_a_shell_is_discovered_with_its_directory() {
    let tree = Tree::spawn("sh", "%claude");
    let expected = tree.agent("claude", Provider::Claude);
    let group = foreground(expected.pid);
    assert_eq!(tree.discover(group), Some(expected.clone()));
    assert!(delegation_process::verify(
        tree.root.id(),
        &tree.generation,
        group,
        &expected
    ));
    if cfg!(target_os = "linux") {
        // Linux requires the actual terminal foreground group.
        assert_eq!(tree.discover(None), None);
        assert_eq!(tree.discover(foreground(tree.root.id())), None);
        assert!(!delegation_process::verify(
            tree.root.id(),
            &tree.generation,
            None,
            &expected
        ));
    }
}

#[test]
fn outer_provider_wins_over_nested_tool_and_provider_children() {
    let tree = Tree::spawn("sh", "%codex>git>claude");
    let expected = tree.agent("codex", Provider::Codex);
    assert_eq!(tree.discover(foreground(expected.pid)), Some(expected));
}

#[test]
fn provider_nested_under_a_non_provider_application_defers() {
    let tree = Tree::spawn("sh", "%git>claude");
    assert_eq!(tree.discover(foreground(tree.pid("git"))), None);
}

#[test]
fn amux_wrapper_and_nested_shells_are_transparent() {
    let tree = Tree::spawn("sh", "amux>sh>%claude");
    let expected = tree.agent("claude", Provider::Claude);
    assert_eq!(tree.discover(foreground(expected.pid)), Some(expected));
}

#[test]
fn shell_only_and_unknown_applications_defer() {
    let shell = Tree::spawn("sh", "");
    assert_eq!(shell.discover(foreground(shell.root.id())), None);
    let tool = Tree::spawn("sh", "%git");
    assert_eq!(tool.discover(foreground(tool.pid("git"))), None);
}

#[test]
fn ambiguous_same_terminal_providers_defer() {
    let tree = Tree::spawn("sh", "claude+codex");
    assert_eq!(tree.discover(foreground(tree.root.id())), None);
    if cfg!(target_os = "linux") {
        // Separate job-control groups: only the actual foreground job is the target.
        let jobs = Tree::spawn("sh", "%claude+%codex");
        let expected = jobs.agent("codex", Provider::Codex);
        assert_eq!(jobs.discover(foreground(expected.pid)), Some(expected));
    }
}

#[test]
fn root_generation_mismatch_and_exit_defer() {
    let mut tree = Tree::spawn("%claude", "");
    let expected = tree.agent("claude", Provider::Claude);
    let group = foreground(expected.pid);
    assert_eq!(tree.discover(group), Some(expected.clone()));
    assert_eq!(
        delegation_process::discover(tree.root.id(), "linux:stale:1", group),
        None
    );
    assert!(!delegation_process::verify(
        tree.root.id(),
        "windows:1",
        group,
        &expected
    ));
    tree.root.kill().unwrap();
    tree.root.wait().unwrap();
    assert_eq!(tree.discover(group), None);
    assert!(!delegation_process::verify(
        expected.pid,
        &expected.generation,
        group,
        &expected
    ));
}

#[test]
fn peer_terminals_are_isolated() {
    let first = Tree::spawn("sh", "%claude");
    let second = Tree::spawn("sh", "%claude");
    let mine = first.agent("claude", Provider::Claude);
    let peer = second.agent("claude", Provider::Claude);
    assert_ne!(mine.pid, peer.pid);
    assert_eq!(first.discover(foreground(mine.pid)), Some(mine.clone()));
    assert_ne!(second.discover(foreground(mine.pid)), Some(mine.clone()));
    assert!(!delegation_process::verify(
        second.root.id(),
        &second.generation,
        foreground(mine.pid),
        &mine
    ));
    assert_eq!(second.discover(foreground(peer.pid)), Some(peer));
}
