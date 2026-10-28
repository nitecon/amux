//! Agent discovery for terminals, and the desktop side of the server event
//! router. The UI owns all terminal access and final input checks; the server
//! decides which agent hears about what (docs/StateMachine.md).
use super::{Desktop, Instant, Message, Provider, Tab, Task, unix_ms};
use amux::{
    delegation::{self, Target},
    delegation_process::{self, AgentProcess},
};
use iced_term::{BackendCommand, Terminal};
use std::{
    collections::{HashMap, HashSet},
    path::PathBuf,
    time::Duration,
};
use tokio::sync::mpsc;
use uuid::Uuid;
#[path = "desktop_events.rs"]
mod events;

#[derive(Default)]
pub(super) struct TabState {
    root: Option<(u32, String)>,
    pub terminal_number: u64,
    pub agent_name: Option<String>,
    pub provider: Option<Provider>,
    pub directory: Option<PathBuf>,
    input: Vec<u8>,
    status: &'static str,
    /// Busy/idle as derived by the server event router, when connected to one.
    pub server_busy: Option<bool>,
}
impl TabState {
    pub fn status(&self) -> &str {
        self.status
    }
    pub fn capture(terminal: &Terminal, terminal_number: u64, directory: &std::path::Path) -> Self {
        Self {
            terminal_number,
            directory: Some(amux::workspace_launch::external_path(
                directory.to_path_buf(),
            )),
            root: terminal
                .child_pid()
                .and_then(|pid| Some((pid, amux::identity::process_generation(pid).ok()?))),
            ..Self::default()
        }
    }
    pub fn note_command(&mut self, command: &BackendCommand) {
        let BackendCommand::Write(bytes) = command else {
            return;
        };
        if matches!(bytes.as_slice(), b"\r" | b"\n" | b"\x03" | b"\x15") {
            self.input.clear();
        } else if contains_text(bytes) || !self.input.is_empty() {
            // Tee the editing stream so restoring also repeats backspace/caret edits.
            // Navigation alone does not establish user text.
            self.input.extend_from_slice(bytes);
        }
    }
}

fn contains_text(bytes: &[u8]) -> bool {
    let mut rest = bytes;
    while let Some((&byte, tail)) = rest.split_first() {
        rest = tail;
        if byte == 0x1b {
            if rest.first() == Some(&b'[') || rest.first() == Some(&b'O') {
                rest = &rest[1..];
                if let Some(end) = rest.iter().position(|b| (0x40..=0x7e).contains(b)) {
                    rest = &rest[end + 1..];
                } else {
                    return false;
                }
            } else {
                // Alt-modified keys are not typed text.
                rest = rest.get(1..).unwrap_or_default();
            }
        } else if byte >= b' ' && byte != 0x7f {
            return true;
        }
    }
    false
}

#[derive(Debug, Clone)]
struct Root {
    workspace: Uuid,
    terminal: u64,
    number: u64,
    pid: u32,
    generation: String,
    foreground: Option<i32>,
    directory: PathBuf,
}
#[derive(Debug, Clone)]
pub(super) struct Found {
    root: Root,
    process: AgentProcess,
    actor: amux::identity::AgentIdentity,
}
#[derive(Debug, Clone)]
struct Candidate {
    actor: amux::identity::AgentIdentity,
    name: String,
    root: Root,
    process: AgentProcess,
    target: Target,
}
#[derive(Debug, Clone)]
pub(super) enum Event {
    Scanned {
        serial: u64,
        found: Vec<Found>,
    },
    Project(PathBuf, Result<String, String>),
    Router(amux::event_client::Incoming),
    /// Native verification result for the delivery in flight.
    Verified(bool),
}

#[derive(Default)]
pub(super) struct Bridge {
    epoch: u64,
    events: events::State,
    status: String,
    candidates: Vec<Candidate>,
    scan_at: Option<Instant>,
    scan_serial: u64,
    scan_inflight: Option<u64>,
    scan_applied: u64,
    pub(super) scan_completed: Option<Instant>,
    projects: HashMap<PathBuf, (Instant, Option<String>)>,
    diagnostic: Option<(String, Instant)>,
}
impl Bridge {
    pub fn stop(&mut self) {
        *self = Self {
            epoch: self.epoch.wrapping_add(1),
            ..Self::default()
        };
    }
}

fn paste(terminal: &Terminal, input: &[u8], message: &str) -> bool {
    let mut bytes = Vec::new();
    if !input.is_empty() {
        // Never synthesize Ctrl+C: it can interrupt a turn or
        // exit the provider when our input tee is stale.
        bytes.extend_from_slice(b"\x1b[F\x15"); // End, Ctrl+U.
    }
    if terminal
        .input_mode()
        .contains(iced_term::TermMode::BRACKETED_PASTE)
    {
        bytes.extend_from_slice(b"\x1b[200~");
        bytes.extend_from_slice(message.as_bytes());
        bytes.extend_from_slice(b"\x1b[201~");
    } else {
        bytes.extend_from_slice(message.as_bytes());
    }
    // End flushes a character-paste burst before Enter, without a timer.
    bytes.extend_from_slice(b"\x1b[F\r");
    bytes.extend_from_slice(input);
    terminal.try_enqueue_input(bytes).is_ok()
}

fn foreground(terminal: &Terminal) -> Option<i32> {
    #[cfg(target_os = "linux")]
    {
        use std::os::fd::AsRawFd;
        // SAFETY: borrowed descriptor stays owned by this exact terminal throughout the query.
        let group = unsafe { libc::tcgetpgrp(terminal.native_terminal_fd().as_raw_fd()) };
        (group > 0).then_some(group)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = terminal;
        None
    }
}
fn tab(desktop: &mut Desktop, id: u64) -> Option<&mut Tab> {
    desktop
        .workspaces
        .iter_mut()
        .flat_map(|w| w.panes.iter_mut().flat_map(|(_, p)| &mut p.tabs))
        .find(|t| t.id == id)
}

/// Bind a terminal number to its actual native executor, without provider hooks.
fn discover_root(root: Root, registry: &std::path::Path) -> Option<Found> {
    use amux::identity::{AgentIdentity, AgentRecord};
    let process = delegation_process::discover(root.pid, &root.generation, root.foreground)?;
    let directory = process.directory.as_deref().unwrap_or(&root.directory);
    let mut actor = AgentIdentity::for_terminal(process.provider, directory, root.number).ok()?;
    let path = registry
        .join("agents")
        .join(format!("{}.json", actor.session_id));
    if let Ok(bytes) = std::fs::read(path)
        && let Ok(record) = serde_json::from_slice::<AgentRecord>(&bytes)
        && record.pid == process.pid
        && record.process_generation == process.generation
        && record.identity.project_id == actor.project_id
        && record.identity.agent_id == actor.agent_id
    {
        actor = record.identity;
    } else {
        actor = actor.resume_at(registry, std::time::Duration::ZERO);
        let record = AgentRecord::inspect(actor.clone(), process.pid).ok()?;
        if record.process_generation != process.generation {
            return None;
        }
        record.register_at(registry).ok()?;
    }
    Some(Found {
        root,
        process,
        actor,
    })
}

impl Desktop {
    fn delegation_roots(&self) -> Vec<Root> {
        let mut roots = Vec::new();
        for workspace in &self.workspaces {
            for (_, pane) in workspace.panes.iter() {
                for tab in &pane.tabs {
                    let Some(terminal) = &tab.terminal else {
                        continue;
                    };
                    let Some((pid, generation)) = &tab.delegation.root else {
                        continue;
                    };
                    roots.push(Root {
                        workspace: workspace.id,
                        terminal: tab.id,
                        number: tab.delegation.terminal_number,
                        pid: *pid,
                        generation: generation.clone(),
                        foreground: foreground(terminal),
                        directory: workspace.directory.clone(),
                    });
                    if roots.len() > delegation::MAX_PENDING {
                        return Vec::new();
                    }
                }
            }
        }
        roots
    }
    fn delegation_scan(&mut self) -> Task<Message> {
        if self.delegation.scan_inflight.is_some() {
            return Task::none();
        }
        let roots = self.delegation_roots();
        let registry = self.registry.clone();
        let epoch = self.delegation.epoch;
        self.delegation.scan_serial += 1;
        let serial = self.delegation.scan_serial;
        self.delegation.scan_inflight = Some(serial);
        self.delegation.scan_at = Some(Instant::now());
        Task::perform(
            async move {
                let found = tokio::task::spawn_blocking(move || {
                    roots
                        .into_iter()
                        .filter_map(|root| discover_root(root, registry.as_deref()?))
                        .collect()
                })
                .await
                .unwrap_or_default();
                Event::Scanned { serial, found }
            },
            move |event| Message::Delegation(epoch, event),
        )
    }
    pub fn delegation_message(&mut self, epoch: u64, event: Event) -> Task<Message> {
        if epoch != self.delegation.epoch {
            return Task::none();
        }
        match event {
            Event::Router(incoming) => return self.events_incoming(incoming),
            Event::Verified(verified) => return self.events_verified(verified),
            Event::Scanned { serial, found } => {
                if self.delegation.scan_inflight == Some(serial) {
                    self.delegation.scan_inflight = None;
                }
                self.queue_diagnostic(format!(
                    "Agent scan: roots={} found={} elapsed_ms={}",
                    self.delegation_roots().len(),
                    found.len(),
                    self.delegation
                        .scan_at
                        .map_or(0, |at| at.elapsed().as_millis())
                ));
                let roots = self.delegation_roots();
                let candidates: Vec<Candidate> = found
                    .into_iter()
                    .filter(|f| {
                        roots.iter().any(|r| {
                            r.workspace == f.root.workspace
                                && r.terminal == f.root.terminal
                                && r.pid == f.root.pid
                                && r.generation == f.root.generation
                                && r.foreground == f.root.foreground
                        })
                    })
                    .map(|f| {
                        let directory = f
                            .process
                            .directory
                            .clone()
                            .unwrap_or_else(|| f.root.directory.clone());
                        let session = f.actor.session_id;
                        let project = self
                            .delegation
                            .projects
                            .get(&directory)
                            .and_then(|(_, p)| p.clone())
                            .unwrap_or_default();
                        Candidate {
                            name: f.actor.display_name(),
                            target: Target {
                                workspace: f.root.workspace,
                                terminal: f.root.terminal,
                                session,
                                agent_id: Some(f.actor.gateway_id()),
                                pid: f.process.pid,
                                generation: f.process.generation.clone(),
                                directory,
                                project,
                                origin: Some(delegation::Origin {
                                    session_id: f.actor.session_id,
                                    instance_id: f.actor.instance_id,
                                    provider: f.actor.provider,
                                    os: f.actor.os.clone(),
                                }),
                                goal: None,
                            },
                            root: f.root,
                            process: f.process,
                            actor: f.actor,
                        }
                    })
                    .collect();
                if serial >= self.delegation.scan_applied {
                    let server_state = self.server_schedules() && self.delegation.events.connected;
                    for workspace in &mut self.workspaces {
                        for (_, pane) in workspace.panes.iter_mut() {
                            for tab in &mut pane.tabs {
                                let candidate =
                                    candidates.iter().find(|c| c.root.terminal == tab.id);
                                if !server_state || candidate.is_none() {
                                    tab.delegation.server_busy = None;
                                }
                                let provider = candidate.map(|c| c.process.provider);
                                let directory = candidate.map(|c| c.target.directory.clone());
                                if tab.delegation.provider != provider
                                    || directory.as_ref().is_some_and(|d| {
                                        tab.delegation.directory.as_ref() != Some(d)
                                    })
                                {
                                    self.dirty = true;
                                }
                                tab.delegation.provider = provider;
                                if let Some(directory) = directory {
                                    tab.delegation.directory = Some(directory);
                                }
                                tab.delegation.agent_name = candidates
                                    .iter()
                                    .find(|c| c.root.terminal == tab.id)
                                    .map(|c| c.name.clone());
                            }
                        }
                    }
                    self.delegation.candidates = candidates;
                    self.delegation.scan_applied = serial;
                    self.delegation.scan_completed = Some(Instant::now());
                }
            }
            Event::Project(directory, result) => {
                let previous = self
                    .delegation
                    .projects
                    .get(&directory)
                    .and_then(|(_, project)| project.clone());
                self.delegation
                    .projects
                    .insert(directory, (Instant::now(), result.ok().or(previous)));
            }
        }
        Task::none()
    }

    fn resolve_projects(&mut self, wanted: bool) -> Vec<Task<Message>> {
        let mut tasks = Vec::new();
        let mut directories = HashSet::new();
        for c in &self.delegation.candidates {
            let directory = c.target.directory.clone();
            if wanted
                && directories.insert(directory.clone())
                && self
                    .delegation
                    .projects
                    .get(&directory)
                    .is_none_or(|(at, _)| at.elapsed() >= Duration::from_secs(30))
                && let Ok(gateway) = self.preferences.current.gateway()
            {
                let previous = self
                    .delegation
                    .projects
                    .get(&directory)
                    .and_then(|(_, p)| p.clone());
                self.delegation
                    .projects
                    .insert(directory.clone(), (Instant::now(), previous));
                let epoch = self.delegation.epoch;
                tasks.push(Task::perform(
                    async move {
                        let result = gateway
                            .resolve_project(directory.clone())
                            .await
                            .map_err(|_| "Repository metadata unavailable".into());
                        Event::Project(directory, result)
                    },
                    move |e| Message::Delegation(epoch, e),
                ));
            }
        }
        tasks
    }

    /// Keep agents and their projects current; on embedded and native servers,
    /// also talk to the event router. A legacy gateway gets nothing automatic.
    pub fn delegation_tick(&mut self) -> Task<Message> {
        let mut tasks = Vec::new();
        if self
            .delegation
            .scan_at
            .is_none_or(|t| t.elapsed() >= Duration::from_secs(1))
        {
            tasks.push(self.delegation_scan());
        }
        if self.server_schedules() {
            tasks.extend(self.resolve_projects(self.preferences.current.automatic_notifications));
            tasks.push(self.events_tick());
        } else if self.delegation.events.running() {
            self.events_stop();
        }
        Task::batch(tasks)
    }
}

impl Desktop {
    pub(super) fn routing_actors(
        &self,
        workspace: Uuid,
    ) -> Vec<(amux::identity::AgentIdentity, Uuid, PathBuf)> {
        if self
            .delegation
            .scan_completed
            .is_none_or(|at| at.elapsed() > Duration::from_secs(2))
        {
            return Vec::new();
        }
        self.delegation
            .candidates
            .iter()
            .filter(|c| {
                c.root.workspace == workspace
                    || self
                        .workspaces
                        .iter()
                        .find(|w| w.id == workspace)
                        .is_some_and(|w| {
                            !c.target.project.is_empty() && c.target.project == w.gateway.project
                        })
            })
            .map(|c| {
                (
                    c.actor.clone(),
                    c.target.executor_generation(),
                    c.target.directory.clone(),
                )
            })
            .collect()
    }
    pub(super) fn routing_name(
        &self,
        target: &amux::task_routing::SessionTarget,
    ) -> Option<String> {
        self.delegation
            .candidates
            .iter()
            .find(|c| {
                c.target.origin.as_ref() == Some(&target.origin)
                    && c.target.executor_generation() == target.generation
            })
            .map(|c| c.name.clone())
    }
    fn refresh_task_project(&mut self, project: &str) -> Task<Message> {
        let mut ready = Vec::new();
        for (index, w) in self
            .workspaces
            .iter_mut()
            .enumerate()
            .filter(|(_, w)| w.gateway.project == project)
        {
            if w.gateway.loading {
                w.gateway.refresh_pending = true;
            } else {
                ready.push(index);
            }
        }
        Task::batch(
            ready
                .into_iter()
                .map(|index| self.query(index))
                .collect::<Vec<_>>(),
        )
    }
    /// Poll gateway-wide presence every 30 seconds while the gateway is used.
    pub(super) fn gateway_stats_tick(&mut self) -> Task<Message> {
        if !self.preferences.current.has_connection()
            || !self.server_schedules()
            || self
                .design
                .gateway_stats_polled
                .is_some_and(|at| at.elapsed() < Duration::from_secs(30))
        {
            return Task::none();
        }
        let Ok(gateway) = self.preferences.current.gateway() else {
            return Task::none();
        };
        self.design.gateway_stats_polled = Some(Instant::now());
        Task::perform(
            async move {
                gateway
                    .presence_summary()
                    .await
                    .map_err(|error| error.to_string())
            },
            Message::GatewayStats,
        )
    }

    /// First status-bar line: connection state, then gateway-wide stats.
    pub(super) fn design_gateway_line(&self) -> String {
        let connection = self.design_connection();
        let Some(stats) = self
            .design
            .gateway_stats
            .as_ref()
            .filter(|_| self.delegation.events.connected)
        else {
            return connection;
        };
        let os = stats
            .os
            .iter()
            .map(|os| match os.as_str() {
                "linux" => "Linux",
                "windows" => "Windows",
                "macos" => "macOS",
                other => other,
            })
            .collect::<Vec<_>>()
            .join(", ");
        let machines = match stats.machines {
            1 => "1 machine".to_owned(),
            n => format!("{n} machines"),
        };
        let mut line = format!("{connection} · {machines}");
        if !os.is_empty() {
            line.push_str(&format!(" · {os}"));
        }
        line.push_str(&format!(
            " · {} agents · {} active",
            stats.agents, stats.active
        ));
        line
    }

    pub(super) fn design_connection(&self) -> String {
        if !self.preferences.current.has_connection() {
            "Gateway: Not configured".into()
        } else if !self.server_schedules() {
            "Gateway: Legacy · no automatic task work".into()
        } else if self.delegation.events.connected {
            "Gateway: Connected".into()
        } else if !self.preferences.current.automatic_notifications {
            "Gateway: Configured · notifications off".into()
        } else {
            "Gateway: Connecting".into()
        }
    }
    pub(super) fn design_task_owner(&self, task: &serde_json::Value) -> String {
        let Some(owner) = task["owner_agent_id"].as_str() else {
            return "Unassigned".into();
        };
        if let Ok(origin) =
            serde_json::from_value::<amux::delegation::Origin>(task["owner_origin"].clone())
            && let Some(name) =
                self.task_agent_choices()
                    .into_iter()
                    .find_map(|choice| match choice {
                        super::desktop_routing::Choice::Session(target, name)
                            if target.origin == origin =>
                        {
                            Some(name)
                        }
                        _ => None,
                    })
        {
            return name;
        }
        self.delegation
            .candidates
            .iter()
            .find(|candidate| {
                candidate
                    .target
                    .origin
                    .as_ref()
                    .is_some_and(|origin| origin.session_id.to_string() == owner)
            })
            .map(|candidate| candidate.name.clone())
            .unwrap_or_else(|| owner.to_owned())
    }
    /// An in-progress task owned by an agent that is not running in this app,
    /// e.g. a teammate on another machine.
    pub(super) fn task_owned_elsewhere(&self, task: &serde_json::Value) -> bool {
        if task["status"] != "in_progress" {
            return false;
        }
        let owner =
            serde_json::from_value::<amux::delegation::Origin>(task["owner_origin"].clone()).ok();
        let agent = task["owner_agent_id"].as_str();
        (owner.is_some() || agent.is_some())
            && !self.delegation.candidates.iter().any(|candidate| {
                candidate
                    .target
                    .origin
                    .as_ref()
                    .is_some_and(|origin| match &owner {
                        Some(owner) => origin == owner,
                        None => {
                            agent.is_some()
                                && (agent == candidate.target.agent_id.as_deref()
                                    || agent == Some(origin.session_id.to_string().as_str()))
                        }
                    })
            })
    }
    pub(super) fn design_owned_tasks(
        &self,
        terminal: u64,
        tasks: &[serde_json::Value],
    ) -> Vec<String> {
        let Some(origin) = self
            .delegation
            .candidates
            .iter()
            .find(|c| c.root.terminal == terminal)
            .and_then(|c| c.target.origin.as_ref())
        else {
            return Vec::new();
        };
        let session = origin.session_id.to_string();
        tasks
            .iter()
            .filter(|t| t["owner_agent_id"] == session && t["status"] == "in_progress")
            .filter_map(|t| t["title"].as_str().map(str::to_owned))
            .collect()
    }
    pub(super) fn design_events(&self) -> Vec<super::desktop_design::EventCard> {
        let mut events = Vec::new();
        if !self.status.is_empty() {
            events.push(super::desktop_design::EventCard {
                key: format!("app:{}", self.status),
                title: "Application".into(),
                detail: self.status.clone(),
                gateway: None,
            });
        }
        if !self.notification_status.is_empty() {
            events.push(super::desktop_design::EventCard {
                key: format!("notifications:{}", self.notification_status),
                title: "Notifications".into(),
                detail: self.notification_status.clone(),
                gateway: None,
            });
        }
        if !self.delegation.status.is_empty() {
            events.push(super::desktop_design::EventCard {
                key: format!("connection:{}", self.delegation.status),
                title: "Connection".into(),
                detail: self.delegation.status.clone(),
                gateway: None,
            });
        }
        if !self.delegation.events.status.is_empty() {
            events.push(super::desktop_design::EventCard {
                key: format!("router:{}", self.delegation.events.status),
                title: "Event router".into(),
                detail: self.delegation.events.status.clone(),
                gateway: None,
            });
        }
        events.extend(self.events_cards());
        events
    }
}

#[cfg(test)]
#[path = "desktop_delegation_tests.rs"]
mod tests;
