//! Desktop side of the server event router (docs/StateMachine.md). On embedded
//! and native servers the server decides who works on what; this desktop only
//! reports its agents and their terminal activity, shows the activity the
//! server derives, and safely delivers `agent.notify` into terminals,
//! reporting each outcome. A legacy gateway has no router, so nothing here runs.
use super::*;
use amux::event_client::{self, Incoming, Subscription};
use serde_json::{Value, json};
use std::collections::VecDeque;

const EVENTS: [&str; 9] = [
    "agent.notify",
    "agent.online",
    "agent.idle",
    "agent.busy",
    "agent.gone",
    // Task panels refresh when their project changes.
    "task.created",
    "task.updated",
    "task.commented",
    "task.completed",
];
const HEARTBEAT: Duration = Duration::from_secs(30);
/// Active terminals refresh their output time well inside the idle window.
const ACTIVITY_REFRESH: Duration = Duration::from_secs(10);
const ACTIVITY_RESEND: Duration = Duration::from_secs(30);
const MAX_AGE_MS: i64 = 5 * 60_000;
const RECENT: usize = 64;
const CARDS: usize = 20;
// Compatibility only: older router servers have no startup-continuity controller.
const RESTART_TASKS: &str = "restart-tasks.json";
const RESTART_WINDOW: Duration = Duration::from_secs(10 * 60);

#[derive(Default)]
pub(super) struct State {
    flight: Option<iced::task::Handle>,
    pub(super) outgoing: Option<mpsc::Sender<Value>>,
    clock: Option<amux::gateway_stream::GatewayClock>,
    targets: Vec<String>,
    pub(super) connected: bool,
    pub(super) server_restart_reminders: Option<bool>,
    pub(super) restart: Vec<Value>,
    restart_loaded: Option<Instant>,
    pub(super) status: String,
    retry_at: Option<Instant>,
    heartbeat_at: Option<Instant>,
    heartbeat_agents: Vec<Value>,
    /// Per agent: active, draft, when reported, and the output time reported.
    activity: HashMap<Uuid, (bool, bool, Instant, Option<i64>)>,
    pub(super) pending: VecDeque<Value>,
    pub(super) delivering: Option<Delivery>,
    delivered: VecDeque<String>,
    /// Recent notifications and their outcomes, for the Events panel.
    cards: VecDeque<(String, delegation::Event, String, String)>,
}

impl State {
    pub(super) fn running(&self) -> bool {
        self.flight.is_some()
    }
}

#[derive(Debug, Clone)]
pub(super) struct Delivery {
    /// A new attempt needs its own fence even when the server replays the same event.
    pub(super) token: Uuid,
    pub(super) event: Value,
    pub(super) candidate: Candidate,
}

impl Desktop {
    /// Append to the bounded `queue.log` in the registry, at most every five seconds.
    pub(in super::super) fn queue_diagnostic(&mut self, message: String) {
        if self
            .delegation
            .diagnostic
            .as_ref()
            .is_some_and(|(last, at)| last == &message || at.elapsed() < Duration::from_secs(5))
        {
            return;
        }
        self.delegation.diagnostic = Some((message.clone(), Instant::now()));
        let Some(directory) = &self.registry else {
            return;
        };
        let path = directory.join("queue.log");
        if std::fs::metadata(&path).is_ok_and(|meta| meta.len() >= 1024 * 1024) {
            let previous = directory.join("queue.previous.log");
            let _ = std::fs::remove_file(&previous);
            if std::fs::rename(&path, previous).is_err() {
                return;
            }
        }
        let mut options = std::fs::OpenOptions::new();
        options.create(true).append(true);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        if let Ok(mut file) = options.open(path) {
            use std::io::Write;
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis();
            let _ = writeln!(file, "{now} {message}");
        }
    }

    /// Embedded and native servers own assignment, review and notifications.
    /// Legacy connections have no automatic controller path.
    pub(super) fn server_schedules(&self) -> bool {
        self.preferences.current.mode != amux::gateway_preferences::GatewayMode::Legacy
    }

    fn events_enabled(&self) -> bool {
        self.preferences.current.automatic_notifications
            && self.preferences.current.has_connection()
            && !self.embedded.busy
            && !self.embedded.stopping
            && (self.preferences.current.mode != amux::gateway_preferences::GatewayMode::Server
                || self.embedded.local.is_some())
    }

    fn events_now(&self) -> i64 {
        self.delegation
            .events
            .clock
            .as_ref()
            .map_or_else(|| unix_ms() as i64, |clock| clock.now_ms())
    }

    pub(in super::super) fn events_stop(&mut self) {
        let old = std::mem::take(&mut self.delegation.events);
        // Reconnecting this logical client keeps notices already acknowledged
        // to the server. Bridge::stop clears them when its connection changes.
        let pending = old
            .delivering
            .map(|delivery| delivery.event)
            .into_iter()
            .chain(old.pending)
            .collect();
        self.delegation.events = State {
            status: old.status,
            retry_at: Some(Instant::now()),
            pending,
            delivered: old.delivered,
            server_restart_reminders: old.server_restart_reminders,
            restart: old.restart,
            restart_loaded: old.restart_loaded,
            ..State::default()
        };
        for workspace in &mut self.workspaces {
            for (_, pane) in workspace.panes.iter_mut() {
                for tab in &mut pane.tabs {
                    tab.delegation.server_busy = None;
                }
            }
        }
    }

    fn events_send(&mut self, frame: Value) {
        if let Some(outgoing) = &self.delegation.events.outgoing
            && outgoing.try_send(frame).is_err()
        {
            self.queue_diagnostic("Event router send queue is full; reconnecting".into());
            self.events_stop();
        }
    }

    fn events_consumer(&self) -> Uuid {
        let Some(directory) = &self.registry else {
            return Uuid::new_v4();
        };
        let path = directory.join("events-consumer");
        if let Some(id) = std::fs::read_to_string(&path)
            .ok()
            .and_then(|text| Uuid::parse_str(text.trim()).ok())
        {
            return id;
        }
        let id = Uuid::new_v4();
        let _ = std::fs::write(&path, id.to_string());
        id
    }

    fn events_targets(&self) -> Vec<String> {
        let mut targets: Vec<String> = self
            .delegation
            .candidates
            .iter()
            .filter(|c| !c.target.project.is_empty())
            .map(|c| c.target.session.to_string())
            .collect();
        targets.sort();
        targets.dedup();
        targets
    }

    fn events_start(&mut self) -> Task<Message> {
        let profile = match self.preferences.current.transport_profile() {
            Ok(profile) => profile,
            Err(error) => {
                self.delegation.events.status = format!("Event router unavailable: {error}");
                self.delegation.events.retry_at = Some(Instant::now());
                return Task::none();
            }
        };
        let subscription = Subscription {
            consumer: self.events_consumer(),
            events: EVENTS.iter().map(|id| id.to_string()).collect(),
            targets: self.events_targets(),
        };
        self.delegation.events.targets = subscription.targets.clone();
        let (outgoing_tx, outgoing_rx) = mpsc::channel(256);
        let (incoming_tx, incoming_rx) = mpsc::channel(64);
        self.delegation.events.outgoing = Some(outgoing_tx);
        let epoch = self.delegation.epoch;
        let stream = iced::stream::channel(
            64,
            move |mut output: iced::futures::channel::mpsc::Sender<Message>| async move {
                use iced::futures::SinkExt;
                let mut incoming_rx = incoming_rx;
                let worker = event_client::run(profile, subscription, outgoing_rx, incoming_tx);
                let forward = async move {
                    while let Some(incoming) = incoming_rx.recv().await {
                        if output
                            .send(Message::Delegation(epoch, Event::Router(incoming)))
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
        self.delegation.events.flight = Some(handle.abort_on_drop());
        task
    }

    /// Called from the delegation tick when the server schedules.
    pub(super) fn events_tick(&mut self) -> Task<Message> {
        if !self.events_enabled() {
            if self.delegation.events.flight.is_some() {
                self.events_stop();
            }
            self.delegation.events.status = if self.preferences.current.automatic_notifications {
                "Waiting for the gateway connection".into()
            } else {
                "Automatic task notifications are disabled in Settings".into()
            };
            return Task::none();
        }
        if self.delegation.events.flight.is_some()
            && self.events_targets() != self.delegation.events.targets
        {
            // The subscription names this desktop's agents; resubscribe for the new set.
            self.events_stop();
            self.delegation.events.retry_at = None;
        }
        if self.delegation.events.flight.is_none() {
            if self
                .delegation
                .events
                .retry_at
                .is_some_and(|at| at.elapsed() < Duration::from_secs(5))
            {
                return Task::none();
            }
            return self.events_start();
        }
        if !self.delegation.events.connected {
            return Task::none();
        }
        self.events_report();
        self.restart_reminders();
        self.events_deliver()
    }

    /// A connection or identity reset must not carry compatibility work into
    /// another server. Ordinary event resubscriptions use events_stop instead.
    pub(in super::super) fn reset_delegation(&mut self) {
        self.delegation.stop();
        if let Some(directory) = &self.registry {
            let _ = std::fs::remove_file(directory.join(RESTART_TASKS));
        }
    }

    /// Save the in-progress tasks owned by this desktop's agents before exit.
    pub(in super::super) fn save_restart_tasks(&self) {
        let Some(directory) = &self.registry else {
            return;
        };
        if self.delegation.events.server_restart_reminders == Some(true) {
            let _ = std::fs::remove_file(directory.join(RESTART_TASKS));
            return;
        }
        let sessions: Vec<String> = self
            .delegation
            .candidates
            .iter()
            .map(|c| c.target.session.to_string())
            .collect();
        let owned: Vec<&Value> = self
            .workspaces
            .iter()
            .flat_map(|w| &w.gateway.tasks)
            .filter(|t| {
                t["status"] == "in_progress"
                    && t["owner_origin"]["session_id"]
                        .as_str()
                        .is_some_and(|s| sessions.iter().any(|own| own == s))
            })
            .collect();
        let path = directory.join(RESTART_TASKS);
        if owned.is_empty() {
            let _ = std::fs::remove_file(path);
        } else if let Ok(json) = serde_json::to_vec(&owned) {
            let _ = std::fs::write(path, json);
        }
    }

    /// Load compatibility work without retiring it until protocol negotiation.
    pub(in super::super) fn load_restart_tasks(&mut self) {
        let Some(path) = self.registry.as_ref().map(|d| d.join(RESTART_TASKS)) else {
            return;
        };
        if let Some(Value::Array(tasks)) = std::fs::read(&path)
            .ok()
            .and_then(|bytes| serde_json::from_slice(&bytes).ok())
        {
            self.delegation.events.restart = tasks;
            self.delegation.events.restart_loaded = Some(Instant::now());
        }
    }

    /// Once a saved task's current state is loaded and it is still owned by a
    /// running agent here, queue a reminder. It waits until that agent is idle,
    /// so an agent that resumed the work is not interrupted.
    pub(super) fn restart_reminders(&mut self) {
        if !self.delegation.events.connected
            || self.delegation.events.server_restart_reminders != Some(false)
        {
            return;
        }
        let events = &mut self.delegation.events;
        if events
            .restart_loaded
            .is_some_and(|at| at.elapsed() > RESTART_WINDOW)
        {
            events.restart.clear();
        }
        if events.restart.is_empty() {
            return;
        }
        let saved = std::mem::take(&mut self.delegation.events.restart);
        let now = self.events_now();
        for task in saved {
            let current = self
                .workspaces
                .iter()
                .filter(|w| w.gateway.loaded_at.is_some())
                .flat_map(|w| &w.gateway.tasks)
                .find(|t| t["id"] == task["id"]);
            let session = task["owner_origin"]["session_id"].clone();
            let running = self
                .delegation
                .candidates
                .iter()
                .find(|c| session.as_str() == Some(c.target.session.to_string().as_str()))
                .cloned();
            let Some(current) = current.filter(|_| running.is_some()) else {
                // Not loaded yet, or its agent has not been found yet.
                self.delegation.events.restart.push(task);
                continue;
            };
            if current["status"] != "in_progress"
                || current["owner_origin"]["session_id"] != session
            {
                continue;
            }
            let Some(candidate) = running else {
                continue;
            };
            let id = current["id"].as_str().unwrap_or_default();
            let instructions = if current["owner_origin"]["instance_id"].as_str()
                == Some(candidate.actor.instance_id.to_string().as_str())
            {
                format!(
                    "You still own this task. Continue it, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux --agent {} tasks user-review {id}.",
                    session.as_str().unwrap_or_default()
                )
            } else {
                "This terminal's previous execution still owns this task. Inspect its prior progress. This is an informational continuity reminder, not an ownership handoff. Use the existing recovery or reassignment flow before attempting task mutations or resuming execution.".to_owned()
            };
            let project = current["project_ident"].clone();
            let hostname = sysinfo::System::host_name();
            // A local notice; its sequence only needs to be a valid event number.
            let mut notice = json!({
                "id": "agent.notify", "uid": Uuid::new_v4().to_string(), "seq": 1,
                "emitted_at": now, "local": true,
                "payload": {
                    "target": session, "project": project, "mode": "assigned", "require_idle": true,
                    "receiver": {"session_id": session,"instance_id": candidate.actor.instance_id,"project":project},
                    "heading": "In-progress task reminder", "instructions": instructions,
                    "event": {"kind": "task_created", "project_ident": project, "created_at": now,
                              "sender_name": "amux", "sender_hostname": hostname, "owner_hostname": hostname,
                              "task": current, "comment": null, "origin": null},
                },
            });
            notice["receiver"] = notification_receiver(&notice, Some(&candidate));
            self.delegation.events.pending.push_back(notice);
        }
    }

    pub(super) fn events_report(&mut self) {
        let now = self.events_now();
        // Presence observes native executors even before repository resolution;
        // task routing remains gated by their resolved project.
        let candidates = self.delegation.candidates.clone();
        let agents: Vec<Value> = candidates
            .iter()
            .map(|c| {
                let mut agent = json!({
                    "session_id": c.target.session.to_string(),
                    "instance_id": c.actor.instance_id.to_string(),
                    "provider": c.actor.provider.command(),
                    "os": c.actor.os,
                    "project": c.target.project,
                    "name": c.name,
                    "agent_id": c.target.agent_id,
                    "generation": c.target.executor_generation().to_string(),
                });
                // A returned live target confirms this execution's repository
                // binding. Unconfirmed reports retain the v1 heartbeat shape.
                if let Some(directory) = self
                    .workspaces
                    .iter()
                    .find(|workspace| {
                        workspace.id == c.target.workspace
                            && workspace.gateway.project == c.target.project
                    })
                    .filter(|workspace| {
                        workspace
                            .gateway
                            .routing
                            .checked
                            .is_some_and(|at| at.elapsed() < Duration::from_secs(60))
                    })
                    .and_then(|workspace| workspace.gateway.routing.directory.as_ref())
                    .filter(|directory| {
                        directory.targets.iter().any(|entry| {
                            entry.expires_at > now
                                && c.target.origin.as_ref() == Some(&entry.target.origin)
                                && entry.target.generation == c.target.executor_generation()
                        })
                    })
                {
                    agent["canonical_remote"] = json!(directory.canonical_remote);
                }
                agent
            })
            .collect();
        let events = &mut self.delegation.events;
        if events.heartbeat_agents != agents
            || events
                .heartbeat_at
                .is_none_or(|at| at.elapsed() >= HEARTBEAT)
        {
            events.heartbeat_at = Some(Instant::now());
            events.heartbeat_agents = agents.clone();
            events.activity.clear();
            let frame = event_client::emit(
                "client.heartbeat",
                json!({"agents": agents, "client_run": self.client_run, "os": std::env::consts::OS}),
                None,
            );
            self.events_send(frame);
        }
        for c in candidates {
            let Some(tab) = tab(self, c.target.terminal) else {
                continue;
            };
            let elapsed = tab.terminal.as_ref().and_then(Terminal::activity_elapsed);
            let active = amux::session::output_active(elapsed);
            let draft = !tab.delegation.input.is_empty();
            let output_at = elapsed.map(|e| now - e.as_millis() as i64);
            let due = self
                .delegation
                .events
                .activity
                .get(&c.target.session)
                .is_none_or(|(was_active, was_draft, at, _)| {
                    *was_active != active
                        || *was_draft != draft
                        || at.elapsed()
                            >= if active {
                                ACTIVITY_REFRESH
                            } else {
                                ACTIVITY_RESEND
                            }
                });
            if due {
                self.delegation
                    .events
                    .activity
                    .insert(c.target.session, (active, draft, Instant::now(), output_at));
                let payload = json!({
                    "session_id": c.target.session.to_string(),
                    "instance_id": c.actor.instance_id.to_string(),
                    "output_at": output_at, "draft": draft,
                });
                self.events_send(event_client::emit("terminal.activity", payload, None));
            }
        }
    }

    pub(super) fn events_incoming(&mut self, incoming: Incoming) -> Task<Message> {
        match incoming {
            Incoming::Connected {
                clock,
                server_restart_reminders,
            } => {
                let events = &mut self.delegation.events;
                events.connected = true;
                events.clock = clock;
                events.heartbeat_at = None;
                events.server_restart_reminders = Some(server_restart_reminders);
                if server_restart_reminders {
                    events.restart.clear();
                    events.pending.retain(|event| event["local"] != true);
                    if events
                        .delivering
                        .as_ref()
                        .is_some_and(|delivery| delivery.event["local"] == true)
                    {
                        events.delivering = None;
                    }
                }
                // Only retire the saved compatibility snapshot after negotiation
                // chooses the authoritative startup-reminder path.
                if let Some(directory) = &self.registry {
                    let _ = std::fs::remove_file(directory.join(RESTART_TASKS));
                }
                events.status = "Connected to the event router".into();
                self.toasts.connection_restored();
            }
            Incoming::Rejected(reason) => {
                self.queue_diagnostic(format!("Event router rejected a frame: {reason}"));
            }
            Incoming::Unsupported => {
                self.delegation.events.connected = false;
                self.delegation.events.status =
                    "This gateway has no event router; automatic task work is off".into();
            }
            Incoming::Lost(reason) => {
                if self.delegation.events.connected {
                    self.toasts.connection_lost();
                }
                self.delegation.events.connected = false;
                self.delegation.events.status = format!("Event router: {reason}; reconnecting");
            }
            Incoming::Event(event) => return self.events_event(event),
        }
        Task::none()
    }

    fn events_event(&mut self, mut event: Value) -> Task<Message> {
        let id = event["id"].as_str().unwrap_or_default();
        if id == "agent.notify" {
            // Acknowledge once queued here, so one agent's wait never holds up
            // the stream; like before the router, pending notices end with the app.
            self.events_send(event_client::ack(&event));
            let candidate = self.delegation.candidates.iter().find(|candidate| {
                event["payload"]["target"].as_str()
                    == Some(candidate.target.session.to_string().as_str())
            });
            event["receiver"] = notification_receiver(&event, candidate);
            let uid = event["uid"].as_str().unwrap_or_default().to_owned();
            let events = &mut self.delegation.events;
            if !events.delivered.contains(&uid)
                && !events
                    .delivering
                    .as_ref()
                    .is_some_and(|delivery| delivery.event["uid"] == event["uid"])
                && !events
                    .pending
                    .iter()
                    .any(|pending| pending["uid"] == event["uid"])
            {
                events.pending.push_back(event);
            }
            return Task::none();
        }
        if id.starts_with("task.") {
            let project = event["payload"]["project"]
                .as_str()
                .unwrap_or_default()
                .to_owned();
            self.events_send(event_client::ack(&event));
            return self.refresh_task_project(&project);
        }
        let session = event["payload"]["agent"]["session_id"]
            .as_str()
            .and_then(|s| Uuid::parse_str(s).ok());
        let busy = match id {
            "agent.idle" => Some(false),
            "agent.busy" => Some(true),
            _ => None,
        };
        if let Some(session) = session {
            let terminals: Vec<u64> = self
                .delegation
                .candidates
                .iter()
                .filter(|c| c.target.session == session)
                .map(|c| c.target.terminal)
                .collect();
            for terminal in terminals {
                if let Some(tab) = tab(self, terminal) {
                    tab.delegation.server_busy = busy;
                    self.dirty = true;
                }
            }
        }
        self.events_send(event_client::ack(&event));
        Task::none()
    }

    fn events_result(&mut self, event: &Value, status: &str, reason: &str) {
        let target = event["payload"]["target"].clone();
        let mut source = event["payload"]["event"].clone();
        source["id"] = event["seq"].clone();
        if let Ok(Some(mut parsed)) = delegation::Event::parse(&source) {
            if let (Some(heading), Some(summary)) = (
                event["payload"]["heading"].as_str(),
                parsed.summary.as_mut(),
            ) {
                summary.heading = heading.into();
            }
            let name = self
                .delegation
                .candidates
                .iter()
                .find(|c| target.as_str() == Some(c.target.session.to_string().as_str()))
                .map_or("Agent".to_owned(), |c| c.name.clone());
            let cards = &mut self.delegation.events.cards;
            cards.push_back((
                event["uid"].as_str().unwrap_or_default().to_owned(),
                parsed,
                name,
                format!(
                    "{status}{}{reason}",
                    if reason.is_empty() { "" } else { " · " }
                ),
            ));
            if cards.len() > CARDS {
                cards.pop_front();
            }
        }
        if event["local"] != true {
            let payload = json!({"target": target, "status": status, "reason": reason});
            let correlation = event["uid"].as_str();
            self.events_send(event_client::emit(
                "agent.notify_result",
                payload,
                correlation,
            ));
        }
        let events = &mut self.delegation.events;
        events
            .delivered
            .push_back(event["uid"].as_str().unwrap_or_default().to_owned());
        if events.delivered.len() > RECENT {
            events.delivered.pop_front();
        }
        self.queue_diagnostic(format!(
            "Event notification {status}: target={} {reason}",
            target.as_str().unwrap_or_default()
        ));
    }

    pub(super) fn events_deliver(&mut self) -> Task<Message> {
        if self.delegation.events.delivering.is_some()
            || self.preferences.open
            || self.workspace_dialog.open
        {
            return Task::none();
        }
        let Some(event) = self.delegation.events.pending.pop_front() else {
            return Task::none();
        };
        let payload = &event["payload"];
        let candidate = self
            .delegation
            .candidates
            .iter()
            .find(|c| payload["target"].as_str() == Some(c.target.session.to_string().as_str()))
            .cloned();
        let Some(candidate) = candidate else {
            self.events_result(&event, "skipped", "agent is not running here");
            return Task::none();
        };
        if !self.events_ready(&event, &candidate) {
            return Task::none();
        }
        let epoch = self.delegation.epoch;
        let c = candidate.clone();
        let token = Uuid::new_v4();
        self.delegation.events.delivering = Some(Delivery {
            token,
            event,
            candidate,
        });
        Task::perform(
            async move {
                let verified = tokio::task::spawn_blocking(move || verify(&c))
                    .await
                    .unwrap_or(false);
                Event::Verified { token, verified }
            },
            move |e| Message::Delegation(epoch, e),
        )
    }

    /// Check changing delivery conditions both before verification and just
    /// before writing to the terminal. Verification yields to other UI events.
    fn events_ready(&mut self, event: &Value, candidate: &Candidate) -> bool {
        let payload = &event["payload"];
        let receiver = &event["receiver"];
        if receiver["session_id"].as_str() != Some(candidate.target.session.to_string().as_str())
            || receiver["instance_id"].as_str()
                != Some(candidate.actor.instance_id.to_string().as_str())
            || receiver["project"].as_str() != Some(candidate.target.project.as_str())
            || receiver["project"] != payload["project"]
            || receiver.get("generation").is_some_and(|generation| {
                generation.as_str()
                    != Some(candidate.target.executor_generation().to_string().as_str())
            })
        {
            self.events_result(event, "skipped", "recipient execution or project changed");
            return false;
        }
        if self.events_now() - event["emitted_at"].as_i64().unwrap_or(0) > MAX_AGE_MS {
            self.events_result(event, "skipped", "notification expired");
            return false;
        }
        let offer = payload["mode"] == "assigned" && event["local"] != true;
        let now = self.events_now();
        let reported = self
            .delegation
            .events
            .activity
            .get(&candidate.target.session)
            .and_then(|(.., output_at)| *output_at);
        let busy = tab(self, candidate.target.terminal).is_none_or(|tab| {
            let elapsed = tab.terminal.as_ref().and_then(Terminal::activity_elapsed);
            let output_at = elapsed.map(|e| now - e.as_millis() as i64);
            !tab.delegation.input.is_empty()
                || match (offer, output_at, reported) {
                    // The server judged this agent idle from the output time we
                    // reported; it is busy only if the terminal wrote since then.
                    (true, Some(output_at), Some(reported)) => output_at > reported + 1_000,
                    (true, None, _) => false,
                    _ => amux::session::output_active(elapsed),
                }
        });
        if payload["require_idle"] == true && busy {
            // A work offer goes back so the server can offer it elsewhere;
            // an informational notice waits for the agent to go quiet.
            if offer {
                self.events_result(event, "skipped", "agent is busy");
            } else {
                self.delegation.events.pending.push_back(event.clone());
            }
            return false;
        }
        true
    }

    /// Finish the delivery in flight once its process has been verified natively.
    pub(super) fn events_verified(&mut self, token: Uuid, verified: bool) -> Task<Message> {
        if self
            .delegation
            .events
            .delivering
            .as_ref()
            .is_none_or(|delivery| delivery.token != token)
        {
            return Task::none();
        }
        let Some(Delivery {
            event, candidate, ..
        }) = self.delegation.events.delivering.take()
        else {
            return Task::none();
        };
        if !verified {
            self.events_result(&event, "failed", "agent process changed");
            return Task::none();
        }
        let Some(current) = self
            .delegation
            .candidates
            .iter()
            .find(|current| current.target.same_attachment(&candidate.target))
            .cloned()
        else {
            self.events_result(&event, "skipped", "recipient attachment changed");
            return Task::none();
        };
        // Native verification belongs to the captured attachment; logical
        // ownership/project may have changed while that verification yielded.
        if !self.events_ready(&event, &current) {
            return Task::none();
        }
        let Some(message) = notification_text(&event, &current) else {
            self.events_result(&event, "skipped", "notification cannot be rendered");
            return Task::none();
        };
        let pasted = tab(self, candidate.target.terminal).is_some_and(|tab| {
            tab.delegation.root.as_ref()
                == Some(&(candidate.root.pid, candidate.root.generation.clone()))
                && tab.terminal.as_ref().is_some_and(|terminal| {
                    foreground(terminal) == candidate.root.foreground
                        && amux::identity::process_generation(candidate.process.pid)
                            .is_ok_and(|g| g == candidate.process.generation)
                        && paste(terminal, &tab.delegation.input, &message)
                })
        });
        if pasted {
            self.events_result(&event, "delivered", "");
        } else {
            self.events_result(&event, "failed", "terminal changed before delivery");
        }
        Task::none()
    }
}

impl Desktop {
    pub(super) fn events_cards(&self) -> Vec<super::super::desktop_design::EventCard> {
        self.delegation
            .events
            .cards
            .iter()
            .rev()
            .map(
                |(uid, event, name, outcome)| super::super::desktop_design::EventCard {
                    key: format!("router:{uid}"),
                    title: event.heading().into(),
                    detail: format!("{}\n{name} · {outcome}", event.terminal_text()),
                    gateway: Some(Box::new(event.clone())),
                },
            )
            .collect()
    }
}

/// The agent's process still matches what the scan bound to its terminal.
pub(super) fn verify(c: &Candidate) -> bool {
    delegation_process::verify(
        c.root.pid,
        &c.root.generation,
        c.root.foreground,
        &c.process,
    )
}

/// Freeze the intended logical execution before a reconnect can replace it.
/// New continuity notices explicitly name their current receiver while keeping
/// the previous owner's task facts intact. Older frames use their owner or
/// requester binding, or the currently observed target when neither is present.
fn notification_receiver(event: &Value, candidate: Option<&Candidate>) -> Value {
    let payload = &event["payload"];
    if let Some(receiver) = payload.get("receiver") {
        return receiver.clone();
    }
    let task = &payload["event"]["task"];
    let mut source = payload["event"].clone();
    source["id"] = event["seq"].clone();
    let requester = delegation::Event::parse(&source)
        .ok()
        .flatten()
        .is_some_and(|event| {
            candidate.is_some_and(|candidate| candidate.target.requester_result(&event))
        });
    let owner = &task["owner_origin"];
    let origin = if requester {
        &task["requester"]["origin"]
    } else if owner["session_id"] == payload["target"] && owner["instance_id"].is_string() {
        owner
    } else if task["requester"]["origin"]["session_id"] == payload["target"] {
        &task["requester"]["origin"]
    } else {
        return candidate.map_or(Value::Null, |candidate| {
            json!({
                "session_id": candidate.target.session,
                "instance_id": candidate.actor.instance_id,
                "project": candidate.target.project,
            })
        });
    };
    let mut receiver = json!({"session_id":origin["session_id"],"instance_id":origin["instance_id"],"project":payload["project"]});
    if requester || origin == &task["requester"]["origin"] && origin != owner {
        receiver["generation"] = task["requester"]["generation"].clone();
    }
    receiver
}

/// The terminal text, rendered by the same formatter as stream notifications.
pub(super) fn notification_text(event: &Value, candidate: &Candidate) -> Option<String> {
    let payload = &event["payload"];
    let mut source = payload["event"].clone();
    source["id"] = event["seq"].clone();
    let mut parsed = delegation::Event::parse(&source).ok()??;
    if let (Some(heading), Some(summary)) = (payload["heading"].as_str(), parsed.summary.as_mut()) {
        summary.heading = heading.into();
    }
    let text = if payload["mode"] == "assigned" {
        parsed.assigned_terminal_text(candidate.target.session, &candidate.name)
    } else {
        parsed.recipient_terminal_text(&candidate.target, &candidate.name)
    };
    Some(match payload["instructions"].as_str() {
        Some(instructions) => format!("{text} {instructions}"),
        None => text,
    })
}
