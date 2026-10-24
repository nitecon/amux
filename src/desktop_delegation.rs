//! CMUX-style lifecycle notifications. The UI owns all terminal access and final input checks.
use super::{Desktop, Element, Instant, Message, Provider, Tab, Task, column, text, unix_ms};
use amux::{
    delegation::{self, Outcome, Target},
    delegation_connection::{self as connection, ConnectionEvent},
    delegation_process::{self, AgentProcess},
    delegation_queue::{Attempt, Queue},
    delegation_readiness::{self, InputState},
};
use iced_term::{BackendCommand, Terminal, TerminalGridObservation};
use sha2::{Digest, Sha256};
use std::{
    collections::{HashMap, HashSet},
    path::PathBuf,
    time::Duration,
};
use tokio::sync::mpsc;
use uuid::Uuid;

#[derive(Default)]
pub(super) struct TabState {
    root: Option<(u32, String)>,
    revision: u64,
    dirty: bool,
    grid: Option<TerminalGridObservation>,
    stable: Option<Instant>,
    wait_for_output: bool,
    status: &'static str,
}
impl TabState {
    pub fn status(&self) -> &str {
        self.status
    }
    pub fn capture(terminal: &Terminal) -> Self {
        Self {
            root: terminal
                .child_pid()
                .and_then(|pid| Some((pid, amux::identity::process_generation(pid).ok()?))),
            ..Self::default()
        }
    }
    pub fn note_command(&mut self, command: &BackendCommand) {
        if let BackendCommand::Write(bytes) = command {
            self.revision = self.revision.wrapping_add(1);
            self.stable = None;
            // Navigation never erases our knowledge of a draft, including whitespace+Home.
            if matches!(bytes.as_slice(), b"\r" | b"\n" | b"\x03" | b"\x15") {
                self.dirty = false;
                self.wait_for_output = true;
            } else if (!bytes.starts_with(b"\x1b[") && !bytes.starts_with(b"\x1bO"))
                || bytes.starts_with(b"\x1b[200~")
            {
                self.dirty = true;
            }
        } else if matches!(command, BackendCommand::MouseReport(..)) {
            self.revision = self.revision.wrapping_add(1);
            self.stable = None;
        }
    }
    fn observe(&mut self, terminal: &Terminal, provider: Provider) -> bool {
        let Some(grid) = terminal.grid_observation(32_768) else {
            self.stable = None;
            self.status = "Waiting for the agent prompt";
            return false;
        };
        if self.grid.as_ref() != Some(&grid) {
            self.grid = Some(grid.clone());
            self.stable = Some(Instant::now());
            self.wait_for_output = false;
        }
        if self.stable.is_none() {
            self.stable = Some(Instant::now());
        }
        let input = delegation_readiness::classify(&grid, provider);
        self.status = if self.dirty || input == InputState::Unfinished {
            "Draft preserved; task notifications are waiting"
        } else if self.wait_for_output || input == InputState::Busy {
            "Agent is busy; task notifications are waiting"
        } else if input == InputState::Unknown {
            "Waiting for a recognized agent prompt"
        } else {
            "Ready for task notifications"
        };
        !self.dirty
            && !self.wait_for_output
            && self
                .stable
                .is_some_and(|t| t.elapsed() >= Duration::from_millis(500))
            && input == InputState::EmptyReady
    }
}

#[derive(Debug, Clone)]
struct Root {
    workspace: Uuid,
    terminal: u64,
    pid: u32,
    generation: String,
    foreground: Option<i32>,
    directory: PathBuf,
}
#[derive(Debug, Clone)]
pub(super) struct Found {
    root: Root,
    process: AgentProcess,
}
#[derive(Debug, Clone)]
struct Candidate {
    root: Root,
    process: AgentProcess,
    target: Target,
    launch_origin: Option<delegation::Origin>,
}
#[derive(Debug, Clone)]
pub(super) struct Proof {
    attempt: Attempt,
    candidate: Candidate,
    revision: u64,
    grid: TerminalGridObservation,
    started: Instant,
}

#[derive(Debug, Clone)]
pub(super) enum Event {
    Stream(ConnectionEvent),
    Scanned {
        serial: u64,
        found: Vec<Found>,
        arrival: Option<delegation::Event>,
    },
    Project(PathBuf, Result<String, String>),
    Verified(Box<Proof>, bool),
}

#[derive(Default)]
pub(super) struct Bridge {
    epoch: u64,
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
    pub fn stop(&mut self) {
        if let Some(queue) = &mut self.queue {
            let _ = queue.refresh(&[], false, unix_ms() as i64);
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

fn bind_candidates(
    registry: &std::path::Path,
    instance: Uuid,
    candidates: &mut [Candidate],
    publish: bool,
) -> anyhow::Result<()> {
    let directory = registry.join("delegation");
    let published = candidates
        .iter()
        .map(|c| amux::delegation_actor::ActorTarget {
            target: c.target.clone(),
            provider: c.process.provider,
        })
        .collect::<Vec<_>>();
    let now = unix_ms() as i64;
    if publish {
        amux::delegation_actor::publish(&directory, instance, &published, now)?;
    }
    let bindings = amux::delegation_actor::bindings(&directory, instance, &published, now)?;
    for candidate in candidates {
        candidate.target.origin = None;
        candidate.target.goal = None;
        if let Some((_, actor)) = bindings
            .iter()
            .find(|(session, _)| *session == candidate.target.session)
        {
            candidate.target.origin = Some(actor.origin.clone());
            candidate.target.goal = actor.goal.clone();
        } else {
            candidate.target.origin = candidate.launch_origin.clone();
        }
    }
    Ok(())
}

impl Desktop {
    fn refresh_actor_bindings(&mut self) -> anyhow::Result<()> {
        if let (Some(registry), Some(queue)) = (&self.registry, &self.delegation.queue) {
            bind_candidates(
                registry,
                queue.journal.consumer,
                &mut self.delegation.candidates,
                false,
            )?;
        }
        Ok(())
    }

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
        let epoch = self.delegation.epoch;
        self.delegation.scan_serial += 1;
        let serial = self.delegation.scan_serial;
        self.delegation.scan_at = Some(Instant::now());
        Task::perform(
            async move {
                let found = tokio::task::spawn_blocking(move || {
                    roots
                        .into_iter()
                        .filter_map(|root| {
                            let process = delegation_process::discover(
                                root.pid,
                                &root.generation,
                                root.foreground,
                            )?;
                            Some(Found { root, process })
                        })
                        .collect()
                })
                .await
                .unwrap_or_default();
                Event::Scanned {
                    serial,
                    found,
                    arrival,
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
        let mut follow = Task::none();
        let result: anyhow::Result<()> = match event {
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
                        event,
                        &targets,
                        self.preferences.current.automatic_notifications,
                        unix_ms() as i64,
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
                let previous = &self.delegation.candidates;
                let roots = self.delegation_roots();
                let mut candidates: Vec<Candidate> = found
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
                        let session = previous
                            .iter()
                            .find(|c| {
                                c.root.terminal == f.root.terminal
                                    && c.root.workspace == f.root.workspace
                                    && c.process == f.process
                            })
                            .map_or_else(Uuid::new_v4, |c| c.target.session);
                        let project = self
                            .delegation
                            .projects
                            .get(&directory)
                            .and_then(|(_, p)| p.clone())
                            .unwrap_or_default();
                        Candidate {
                            target: Target {
                                workspace: f.root.workspace,
                                terminal: f.root.terminal,
                                session,
                                pid: f.process.pid,
                                generation: f.process.generation.clone(),
                                directory,
                                project,
                                origin: None,
                                goal: None,
                            },
                            launch_origin: previous
                                .iter()
                                .find(|c| {
                                    c.root.terminal == f.root.terminal
                                        && c.root.workspace == f.root.workspace
                                        && c.process == f.process
                                })
                                .and_then(|c| c.launch_origin.clone()),
                            root: f.root,
                            process: f.process,
                        }
                    })
                    .collect();
                if let Some(registry) = self.registry.clone() {
                    for candidate in &mut candidates {
                        if candidate.launch_origin.is_none()
                            && let Some(tab) = tab(self, candidate.target.terminal)
                            && let Some(recipient) = &mut tab.recipient
                            && let Ok(actor) = recipient.resolve(&registry)
                        {
                            candidate.launch_origin = Some(delegation::Origin {
                                session_id: actor.session_id,
                                instance_id: actor.instance_id,
                                provider: actor.provider,
                                os: actor.os.clone(),
                            });
                        }
                    }
                }
                if let (Some(registry), Some(queue)) = (&self.registry, &self.delegation.queue)
                    && let Err(error) =
                        bind_candidates(registry, queue.journal.consumer, &mut candidates, true)
                {
                    self.delegation.fail(error);
                    return Task::none();
                }
                let targets = candidates
                    .iter()
                    .map(|c| c.target.clone())
                    .collect::<Vec<_>>();
                let receive = if let Some(event) = arrival {
                    self.delegation.queue.as_mut().map_or(Ok(()), |q| {
                        q.receive(
                            event,
                            &targets,
                            self.preferences.current.automatic_notifications,
                            unix_ms() as i64,
                        )
                    })
                } else {
                    Ok(())
                };
                if serial >= self.delegation.scan_applied {
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
                if let Err(error) = self.refresh_actor_bindings() {
                    self.delegation.fail(error);
                    return Task::none();
                }
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
                    && proof.attempt.event.fresh(unix_ms() as i64)
                    && current.is_some()
                    && let Some(tab) = tab(self, proof.attempt.target.terminal)
                    && let Some(terminal) = &tab.terminal
                {
                    // All checks below run synchronously on the UI owner, with no await or callback.
                    let state = &mut tab.delegation;
                    if state.root.as_ref()
                        == Some(&(
                            proof.candidate.root.pid,
                            proof.candidate.root.generation.clone(),
                        ))
                        && foreground(terminal) == proof.candidate.root.foreground
                        && amux::identity::process_generation(proof.candidate.process.pid)
                            .is_ok_and(|g| g == proof.candidate.process.generation)
                        && state.revision == proof.revision
                        && !state.dirty
                        && !state.wait_for_output
                        && terminal.grid_observation(32_768).as_ref() == Some(&proof.grid)
                        && delegation_readiness::classify(
                            &proof.grid,
                            proof.candidate.process.provider,
                        ) == InputState::EmptyReady
                    {
                        let message = proof.attempt.event.terminal_text();
                        let bytes = if proof
                            .grid
                            .observation
                            .mode
                            .contains(iced_term::TermMode::BRACKETED_PASTE)
                        {
                            format!("\x1b[200~{message}\x1b[201~\r").into_bytes()
                        } else {
                            format!("{message}\r").into_bytes()
                        };
                        outcome = match terminal.try_enqueue_input(bytes) {
                            Ok(()) => Outcome::Submitted,
                            Err(_) => Outcome::Uncertain,
                        };
                        state.wait_for_output = true;
                        state.stable = None;
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
        follow
    }

    pub fn delegation_tick(&mut self) -> Task<Message> {
        if !self.delegation.initialized {
            return self.start_delegation();
        }
        if self.delegation.queue.is_none() || self.delegation.commands.is_none() {
            return Task::none();
        }
        let mut tasks = Vec::new();
        if self
            .delegation
            .scan_at
            .is_none_or(|t| t.elapsed() >= Duration::from_secs(1))
        {
            tasks.push(self.delegation_scan(None));
        }
        let mut directories = HashSet::new();
        for c in &self.delegation.candidates {
            let directory = c.target.directory.clone();
            if self.preferences.current.automatic_notifications
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
        if let Err(error) = self.refresh_actor_bindings() {
            self.delegation.fail(error);
            return Task::batch(tasks);
        }
        let targets = self.delegation.targets();
        if let Some(q) = &mut self.delegation.queue
            && let Err(error) = q.refresh(
                &targets,
                self.preferences.current.automatic_notifications,
                unix_ms() as i64,
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
            if let Some(tab) = tab(self, c.target.terminal)
                && let Some(terminal) = &tab.terminal
                && tab.delegation.observe(terminal, c.process.provider)
            {
                ready.insert(c.target.session);
            }
        }
        let attempt = self.delegation.queue.as_ref().and_then(|q| q.next(&ready));
        if let Some(attempt) = attempt {
            let Some(candidate) = candidates.into_iter().find(|c| c.target == attempt.target)
            else {
                return Task::batch(tasks);
            };
            let Some(tab) = tab(self, attempt.target.terminal) else {
                return Task::batch(tasks);
            };
            let Some(grid) = tab.delegation.grid.clone() else {
                return Task::batch(tasks);
            };
            let proof = Proof {
                attempt,
                candidate,
                revision: tab.delegation.revision,
                grid,
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
    pub fn delegation_view(&self) -> Element<'_, Message> {
        let pending = self.delegation.queue.as_ref().map_or(0, |q| {
            q.journal
                .receipts
                .iter()
                .flat_map(|r| &r.recipients)
                .filter(|r| !r.outcome.terminal())
                .count()
        });
        let mut view = column![
            text(format!(
                "{} · {} agents · {pending} pending",
                self.delegation.status,
                self.delegation.candidates.len()
            ))
            .size(13)
        ]
        .spacing(4);
        if let Some(q) = &self.delegation.queue {
            for r in q.journal.receipts.iter().rev().take(8) {
                view = view.push(
                    text(format!(
                        "{} · {}{}",
                        r.event.task,
                        r.status(),
                        if r.confirmed { " · recorded" } else { "" }
                    ))
                    .size(12),
                );
            }
        }
        view.into()
    }
}

#[cfg(test)]
#[path = "desktop_delegation_tests.rs"]
mod tests;
