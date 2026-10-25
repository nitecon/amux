//! CMUX-style lifecycle notifications. The UI owns all terminal access and final input checks.
use super::{Desktop, Instant, Message, Provider, Tab, Task, unix_ms};
use amux::{
    delegation::{self, Outcome, Target},
    delegation_connection::{self as connection, ConnectionEvent},
    delegation_process::{self, AgentProcess},
    delegation_queue::{Attempt, Queue},
};
use iced_term::{BackendCommand, Terminal};
use sha2::{Digest, Sha256};
use std::{
    collections::{HashMap, HashSet},
    path::PathBuf,
    time::Duration,
};
use tokio::sync::mpsc;
use uuid::Uuid;
#[path = "desktop_autoreview.rs"]
mod autoreview;
#[path = "desktop_backlog.rs"]
mod backlog;

#[derive(Default)]
pub(super) struct TabState {
    root: Option<(u32, String)>,
    pub terminal_number: u64,
    pub agent_name: Option<String>,
    pub provider: Option<Provider>,
    pub directory: Option<PathBuf>,
    input: Vec<u8>,
    status: &'static str,
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
pub(super) struct Proof {
    attempt: Attempt,
    candidate: Candidate,
    started: Instant,
}

#[derive(Debug, Clone)]
pub(super) enum Event {
    AutoreviewPrepared(Uuid, Result<Option<Box<autoreview::Prepared>>, String>),
    BacklogVerified(Box<backlog::Pick>, bool),
    BacklogClaimed(Uuid, Result<Option<serde_json::Value>, String>),
    Stream(ConnectionEvent),
    Scanned {
        serial: u64,
        found: Vec<Found>,
        arrival: Option<Box<delegation::Event>>,
    },
    Project(PathBuf, Result<String, String>),
    Verified(Box<Proof>, bool),
}

#[derive(Default)]
pub(super) struct Bridge {
    autoreview: autoreview::State,
    backlog: backlog::State,
    epoch: u64,
    clock: Option<amux::gateway_stream::GatewayClock>,
    initialized: bool,
    queue: Option<Queue>,
    flight: Option<iced::task::Handle>,
    commands: Option<mpsc::Sender<connection::Command>>,
    connection: Option<u64>,
    status: String,
    candidates: Vec<Candidate>,
    scan_at: Option<Instant>,
    scan_serial: u64,
    scan_applied: u64,
    scan_completed: Option<Instant>,
    projects: HashMap<PathBuf, (Instant, Option<String>)>,
    verifying: bool,
}
impl Bridge {
    pub(super) fn reconcile_backlog(&mut self, project: &str, tasks: &[serde_json::Value]) {
        if let Some(queue) = &mut self.queue
            && let Err(error) = queue.reconcile_backlog(project, tasks)
        {
            self.fail(error);
        }
    }
    fn now_ms(&self) -> i64 {
        self.clock
            .as_ref()
            .map_or_else(|| unix_ms() as i64, |clock| clock.now_ms())
    }
    pub fn stop(&mut self) {
        let now = self.now_ms();
        if let Some(queue) = &mut self.queue {
            let _ = queue.refresh(&[], false, now);
        }
        self.flight.take();
        *self = Self {
            epoch: self.epoch.wrapping_add(1),
            ..Self::default()
        };
    }
    fn fail(&mut self, error: impl std::fmt::Display) {
        self.status = format!("Task notifications paused: {error}");
        self.flight.take();
        self.commands = None;
        self.connection = None;
    }
    fn targets(&self) -> Vec<Target> {
        self.candidates.iter().map(|c| c.target.clone()).collect()
    }
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
    fn delegation_scan(&mut self, arrival: Option<delegation::Event>) -> Task<Message> {
        let roots = self.delegation_roots();
        let registry = self.registry.clone();
        let epoch = self.delegation.epoch;
        self.delegation.scan_serial += 1;
        let serial = self.delegation.scan_serial;
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
                Event::Scanned {
                    serial,
                    found,
                    arrival: arrival.map(Box::new),
                }
            },
            move |event| Message::Delegation(epoch, event),
        )
    }
    fn start_delegation(&mut self) -> Task<Message> {
        self.delegation.initialized = true;
        if self.preferences.current.url.is_empty() {
            self.delegation.status = "Enter a gateway URL and API key".into();
            return Task::none();
        }
        if self.ledger.is_none() {
            self.delegation
                .fail("exclusive delivery storage unavailable");
            return Task::none();
        }
        let Some(directory) = &self.registry else {
            return Task::none();
        };
        let config = &self.preferences.current;
        let namespace = format!(
            "{:x}",
            Sha256::digest(format!("{}\0{}", config.url, config.api_key).as_bytes())
        );
        let queue = match Queue::open(directory.join("delegation.json"), &namespace) {
            Ok(queue) => queue,
            Err(error) => {
                self.delegation.fail(error);
                return Task::none();
            }
        };
        let consumer = queue.journal.consumer;
        let cursor = queue.journal.cursor;
        self.delegation.queue = Some(queue);
        let (commands_tx, commands_rx) = mpsc::channel(64);
        let (events_tx, events_rx) = mpsc::channel(64);
        self.delegation.commands = Some(commands_tx);
        let url = config.url.clone();
        let key = config.api_key.clone();
        let epoch = self.delegation.epoch;
        let stream = iced::stream::channel(
            64,
            move |mut output: iced::futures::channel::mpsc::Sender<Message>| async move {
                use iced::futures::SinkExt;
                let mut events_rx = events_rx;
                let worker = connection::run(url, key, consumer, cursor, commands_rx, events_tx);
                let forward = async move {
                    while let Some(event) = events_rx.recv().await {
                        if output
                            .send(Message::Delegation(epoch, Event::Stream(event)))
                            .await
                            .is_err()
                        {
                            break;
                        }
                    }
                };
                tokio::select! {_ = worker => {}, _ = forward => {}}
            },
        );
        let (task, handle) = Task::run(stream, |message| message).abortable();
        self.delegation.flight = Some(handle.abort_on_drop());
        task
    }
    fn delegation_acknowledge(&mut self) {
        let Some(connection) = self.delegation.connection else {
            return;
        };
        let Some(commands) = &self.delegation.commands else {
            return;
        };
        if commands.capacity() < 32 {
            return;
        }
        let Some(queue) = &mut self.delegation.queue else {
            return;
        };
        for frame in queue.acknowledgments() {
            if commands
                .try_send(connection::Command { connection, frame })
                .is_err()
            {
                self.delegation
                    .fail("gateway acknowledgment channel closed");
                break;
            }
        }
    }
    pub fn delegation_message(&mut self, epoch: u64, event: Event) -> Task<Message> {
        if epoch != self.delegation.epoch {
            return Task::none();
        }
        let now = self.delegation.now_ms();
        let mut follow = Task::none();
        let mut refresh = Task::none();
        let result: anyhow::Result<()> = match event {
            Event::AutoreviewPrepared(request, result) => {
                return self.autoreview_prepared(request, result);
            }
            Event::BacklogVerified(pick, valid) => return self.backlog_verified(*pick, valid),
            Event::BacklogClaimed(request, result) => return self.backlog_claimed(request, result),
            Event::Stream(ConnectionEvent::Clock(clock)) => {
                self.delegation.clock = Some(clock);
                Ok(())
            }
            Event::Stream(ConnectionEvent::State(status)) => {
                self.delegation.status = status;
                self.delegation.connection = None;
                if let Some(q) = &mut self.delegation.queue {
                    q.disconnected();
                }
                Ok(())
            }
            Event::Stream(ConnectionEvent::Subscribed {
                connection,
                cursor,
                replay_through,
            }) => {
                self.delegation.connection = Some(connection);
                self.delegation
                    .queue
                    .as_mut()
                    .map_or(Ok(()), |q| q.subscribed(cursor, replay_through))
            }
            Event::Stream(ConnectionEvent::Event(event)) => {
                refresh = self.refresh_task_project(&event.project);
                // Pin the last completed native scan at arrival, rather than spawning an
                // unbounded scan task for every stream event or attaching agents started later.
                let targets = if self
                    .delegation
                    .scan_completed
                    .is_some_and(|at| at.elapsed() <= Duration::from_secs(2))
                {
                    self.delegation.targets()
                } else {
                    Vec::new()
                };
                self.delegation.queue.as_mut().map_or(Ok(()), |q| {
                    q.receive(
                        *event,
                        &targets,
                        self.preferences.current.automatic_notifications,
                        now,
                    )
                })
            }
            Event::Stream(ConnectionEvent::Rejected { event_id, reason }) => self
                .delegation
                .queue
                .as_mut()
                .map_or(Ok(()), |q| q.reject(event_id, &reason)),
            Event::Stream(ConnectionEvent::Recorded {
                event_id, status, ..
            }) => self
                .delegation
                .queue
                .as_mut()
                .map_or(Ok(()), |q| q.recorded(event_id, &status)),
            Event::Scanned {
                serial,
                found,
                arrival,
            } => {
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
                let targets = candidates
                    .iter()
                    .map(|c| c.target.clone())
                    .collect::<Vec<_>>();
                let receive = if let Some(event) = arrival {
                    self.delegation.queue.as_mut().map_or(Ok(()), |q| {
                        q.receive(
                            *event,
                            &targets,
                            self.preferences.current.automatic_notifications,
                            now,
                        )
                    })
                } else {
                    Ok(())
                };
                if serial >= self.delegation.scan_applied {
                    for workspace in &mut self.workspaces {
                        for (_, pane) in workspace.panes.iter_mut() {
                            for tab in &mut pane.tabs {
                                let candidate =
                                    candidates.iter().find(|c| c.root.terminal == tab.id);
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
                receive
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
                Ok(())
            }
            Event::Verified(proof, verified) => {
                self.delegation.verifying = false;
                let current = self
                    .delegation
                    .candidates
                    .iter()
                    .find(|c| c.target == proof.attempt.target)
                    .cloned();
                let approved = self.preferences.current.automatic_notifications
                    && self.delegation.connection.is_some();
                if !self
                    .delegation
                    .queue
                    .as_ref()
                    .is_some_and(|q| q.submitting(&proof.attempt))
                {
                    return Task::none();
                }
                let mut outcome = Outcome::Skipped;
                if verified
                    && approved
                    && proof.started.elapsed() <= Duration::from_secs(2)
                    && proof.attempt.event.fresh(now)
                    && current.is_some()
                    && let Some(tab) = tab(self, proof.attempt.target.terminal)
                    && let Some(terminal) = &tab.terminal
                {
                    if tab.delegation.root.as_ref()
                        == Some(&(
                            proof.candidate.root.pid,
                            proof.candidate.root.generation.clone(),
                        ))
                        && foreground(terminal) == proof.candidate.root.foreground
                        && amux::identity::process_generation(proof.candidate.process.pid)
                            .is_ok_and(|g| g == proof.candidate.process.generation)
                    {
                        // One ordered native input transaction, owned by this UI turn.
                        // User edits cannot interleave with clear/send/submit/restore.
                        let mut bytes = Vec::new();
                        if !tab.delegation.input.is_empty() {
                            // Never synthesize Ctrl+C: it can interrupt a turn or
                            // exit the provider when our input tee is stale.
                            bytes.extend_from_slice(b"\x1b[F\x15"); // End, Ctrl+U.
                        }
                        let message = proof.attempt.event.assigned_terminal_text(
                            proof.attempt.target.session,
                            &proof.candidate.name,
                        );
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
                        bytes.extend_from_slice(&tab.delegation.input);
                        outcome = match terminal.try_enqueue_input(bytes) {
                            Ok(()) => Outcome::Submitted,
                            Err(_) => Outcome::Uncertain,
                        };
                    } else {
                        outcome = Outcome::Queued;
                    }
                }
                self.delegation
                    .queue
                    .as_mut()
                    .map_or(Ok(()), |q| q.finish(&proof.attempt, outcome))
            }
        };
        if let Err(error) = result {
            self.delegation.fail(error);
            follow = Task::none();
        }
        self.delegation_acknowledge();
        Task::batch([follow, refresh])
    }

    pub fn delegation_tick(&mut self) -> Task<Message> {
        let now = self.delegation.now_ms();
        if !self.delegation.initialized {
            return self.start_delegation();
        }
        let mut tasks = Vec::new();
        if self
            .delegation
            .scan_at
            .is_none_or(|t| t.elapsed() >= Duration::from_secs(1))
        {
            tasks.push(self.delegation_scan(None));
        }
        if self.delegation.queue.is_none() || self.delegation.commands.is_none() {
            return Task::batch(tasks);
        }
        let mut directories = HashSet::new();
        for c in &self.delegation.candidates {
            let directory = c.target.directory.clone();
            if (self.preferences.current.automatic_notifications
                || self.preferences.current.autoreview)
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
        let targets = self.delegation.targets();
        if let Some(q) = &mut self.delegation.queue
            && let Err(error) = q.refresh(
                &targets,
                self.preferences.current.automatic_notifications,
                now,
            )
        {
            self.delegation.fail(error);
            return Task::batch(tasks);
        }
        self.delegation_acknowledge();
        if self.delegation.verifying
            || !self.preferences.current.automatic_notifications
            || self.delegation.connection.is_none()
        {
            return Task::batch(tasks);
        }
        let mut ready = HashSet::new();
        let candidates = self.delegation.candidates.clone();
        for c in &candidates {
            if amux::autoreview::reviewing(&c.target.directory, &c.actor) {
                continue;
            }
            if let Some(tab) = tab(self, c.target.terminal)
                && tab.terminal.is_some()
            {
                tab.delegation.status = "Ready for task notifications";
                ready.insert(c.target.session);
            }
        }
        let attempt = self.delegation.queue.as_ref().and_then(|q| q.next(&ready));
        if let Some(attempt) = attempt {
            let Some(candidate) = candidates.into_iter().find(|c| c.target == attempt.target)
            else {
                return Task::batch(tasks);
            };
            let proof = Proof {
                attempt,
                candidate,
                started: Instant::now(),
            };
            if let Some(q) = &mut self.delegation.queue
                && let Err(error) = q.begin(&proof.attempt)
            {
                self.delegation.fail(error);
                return Task::batch(tasks);
            }
            self.delegation.verifying = true;
            let epoch = self.delegation.epoch;
            tasks.push(Task::perform(
                async move {
                    let c = proof.candidate.clone();
                    let verified = tokio::task::spawn_blocking(move || {
                        delegation_process::verify(
                            c.root.pid,
                            &c.root.generation,
                            c.root.foreground,
                            &c.process,
                        )
                    })
                    .await
                    .unwrap_or(false);
                    Event::Verified(Box::new(proof), verified)
                },
                move |e| Message::Delegation(epoch, e),
            ));
        }
        Task::batch(tasks)
    }
}

impl Desktop {
    pub(super) fn backlog_status(&self) -> &str {
        &self.delegation.backlog.status
    }
    pub(super) fn autoreview_status(&self) -> &str {
        &self.delegation.autoreview.status
    }
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
    pub(super) fn design_connection(&self) -> String {
        if self.delegation.connection.is_some() {
            "Gateway: Connected".into()
        } else if self.preferences.current.url.is_empty() {
            "Gateway: Not configured".into()
        } else if !self.preferences.current.automatic_notifications {
            "Gateway: Configured · notifications off".into()
        } else {
            "Gateway: Disconnected".into()
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
        if let Some(queue) = &self.delegation.queue {
            let mut history = queue
                .journal
                .recent
                .iter()
                .chain(queue.journal.receipts.iter().map(|r| &r.event))
                .collect::<Vec<_>>();
            history.sort_by_key(|e| e.id);
            history.dedup_by_key(|e| e.id);
            for event in history {
                let receipt = queue
                    .journal
                    .receipts
                    .iter()
                    .find(|r| r.event.id == event.id);
                let mut detail = event.terminal_text();
                if let Some(receipt) = receipt {
                    detail.push_str(&format!(
                        "\nDelivery: {}{} · {}",
                        receipt.status(),
                        if receipt.confirmed { " · saved" } else { "" },
                        receipt.reason
                    ));
                    for recipient in &receipt.recipients {
                        let name = self
                            .delegation
                            .candidates
                            .iter()
                            .find(|c| c.target.session == recipient.target.session)
                            .map(|c| c.name.as_str())
                            .unwrap_or("Previous agent");
                        detail.push_str(&format!("\n{name} · {}", recipient.reason));
                    }
                }
                events.push(super::desktop_design::EventCard {
                    key: format!("gateway:{}", event.id),
                    title: event.heading().into(),
                    detail,
                    gateway: Some(Box::new(event.clone())),
                });
            }
        }
        events
    }
}

#[cfg(test)]
#[path = "desktop_delegation_tests.rs"]
mod tests;
