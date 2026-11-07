//! Desktop side of the server event router (docs/StateMachine.md). On embedded
//! and native servers the server decides who works on what; this desktop only
//! reports its agents and their terminal activity, shows the activity the
//! server derives, and safely delivers `agent.notify` into terminals,
//! reporting each outcome.
use super::*;
use amux::event_client::{self, Incoming, Subscription};
use serde_json::{Value, json};
use std::collections::VecDeque;
#[path = "desktop_guidance.rs"]
mod guidance;

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
#[derive(Default)]
pub(super) struct State {
    flight: Option<iced::task::Handle>,
    pub(super) outgoing: Option<mpsc::Sender<Value>>,
    clock: Option<amux::gateway_stream::GatewayClock>,
    targets: Vec<String>,
    pub(super) connected: bool,
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
    guidance_seen: HashMap<Uuid, (String, u64)>,
    last_delivery: HashMap<Uuid, Instant>,
    guidance_cards: VecDeque<super::super::desktop_design::EventCard>,
}

#[derive(Debug, Clone)]
pub(super) struct Delivery {
    /// A new attempt needs its own fence even when the server replays the same event.
    pub(super) token: Uuid,
    pub(super) event: Value,
    pub(super) candidate: Candidate,
    pub(super) stage: DeliveryStage,
}

pub(super) const CLEAR_WAIT: Duration = Duration::from_secs(5);

#[derive(Debug, Clone, Copy)]
pub(super) enum DeliveryStage {
    Verifying,
    Clearing {
        started: Instant,
        input_revision: u64,
    },
    ReadyAfterClear {
        input_revision: u64,
    },
    VerifyingAfterClear {
        input_revision: u64,
    },
}

#[derive(Clone, Copy)]
enum DeliveryInput<'a> {
    Clear,
    Notification(&'a str),
}

fn new_task_context(event: &Value, idle: Option<Duration>, has_draft: bool) -> bool {
    let payload = &event["payload"];
    (payload["new_task"] == true || payload["clear_context"] == true)
        && payload["event"]["kind"] == "task_created"
        && payload["event"]["comment"].is_null()
        && payload["event"]["task"]["id"]
            .as_str()
            .is_some_and(|id| Uuid::parse_str(id).is_ok())
        && (payload["clear_context"] == true
            || !payload["event"]["task"]["labels"]
                .as_array()
                .is_some_and(|labels| labels.iter().any(|label| label == "task-review")))
        && idle.is_some_and(|elapsed| elapsed >= amux::session::IDLE_AFTER)
        && !has_draft
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
        // A reconnect must not repeat /clear or lose its five-second wait.
        let (delivering, interrupted) = match old.delivering {
            Some(delivery) if matches!(delivery.stage, DeliveryStage::Verifying) => {
                (None, Some(delivery.event))
            }
            delivering => (delivering, None),
        };
        let pending = interrupted.into_iter().chain(old.pending).collect();
        self.delegation.events = State {
            status: old.status,
            retry_at: Some(Instant::now()),
            pending,
            delivering,
            delivered: old.delivered,
            guidance_seen: old.guidance_seen,
            last_delivery: old.last_delivery,
            guidance_cards: old.guidance_cards,
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
        self.guidance_tick();
        if !self.events_enabled() {
            if self.delegation.events.flight.is_some() {
                self.events_stop();
            }
            self.delegation.events.status = if self.preferences.current.automatic_notifications {
                "Waiting for the gateway connection".into()
            } else {
                "Automatic task notifications are disabled in Settings".into()
            };
            return self.events_deliver();
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
                return self.events_deliver();
            }
            let start = self.events_start();
            return Task::batch([start, self.events_deliver()]);
        }
        if !self.delegation.events.connected {
            return self.events_deliver();
        }
        self.events_report();
        self.events_deliver()
    }

    /// A connection or identity reset cannot carry queued work to another server.
    pub(in super::super) fn reset_delegation(&mut self) {
        self.delegation.stop();
        self.clear_project_inventory();
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
                    "review_context_clear": true,
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
                json!({"agents": agents, "client_run": self.client_run, "os": std::env::consts::OS,
                    "hostname": sysinfo::System::host_name()}),
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
            Incoming::Connected { clock, .. } => {
                let events = &mut self.delegation.events;
                events.connected = true;
                events.clock = clock;
                events.heartbeat_at = None;
                events.status = "Connected to the event router".into();
                self.toasts.connection_restored();
            }
            Incoming::Rejected(reason) => {
                self.queue_diagnostic(format!("Event router rejected a frame: {reason}"));
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
            // New work takes priority over background guidance and recall.
            if assignment_task(&event).is_some() {
                if self
                    .delegation
                    .events
                    .delivering
                    .as_ref()
                    .is_some_and(|d| d.event["amux_guidance"].is_string())
                    && let Some(delivery) = self.delegation.events.delivering.take()
                {
                    self.events_result(&delivery.event, "skipped", "new task takes priority");
                }
                let mut skipped = Vec::new();
                self.delegation.events.pending.retain(|e| {
                    if e["amux_guidance"].is_string() {
                        skipped.push(e.clone());
                        false
                    } else {
                        true
                    }
                });
                for skipped in skipped {
                    self.events_result(&skipped, "skipped", "new task takes priority");
                }
            }
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
        if event["amux_guidance"].is_string() {
            let detail = format!(
                "{} {status}: target={} {reason}",
                event["amux_guidance"], event["payload"]["target"]
            );
            if event["amux_guidance"] == "review"
                && let (Some(registry), Some(session), Some(cycle)) = (
                    self.registry.as_deref(),
                    event["payload"]["target"]
                        .as_str()
                        .and_then(|s| Uuid::parse_str(s).ok()),
                    event["amux_cycle"]
                        .as_str()
                        .and_then(|s| Uuid::parse_str(s).ok()),
                )
                && let Err(error) = amux::guidance::with_session(registry, session, |state| {
                    state.review_result(cycle, &detail)
                })
            {
                self.queue_diagnostic(format!(
                    "Memory review outcome could not be saved: {error:#}"
                ));
            }
            let cards = &mut self.delegation.events.guidance_cards;
            cards.push_back(super::super::desktop_design::EventCard {
                key: event["uid"].as_str().unwrap_or_default().to_owned(),
                title: "Session guidance".into(),
                detail,
                gateway: None,
            });
            if cards.len() > CARDS {
                cards.pop_front();
            }
            self.queue_diagnostic(format!(
                "Session guidance {} {status}: target={} {reason}",
                event["amux_guidance"], event["payload"]["target"]
            ));
            return;
        }
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
        let payload = json!({"target": target, "status": status, "reason": reason});
        let correlation = event["uid"].as_str();
        self.events_send(event_client::emit(
            "agent.notify_result",
            payload,
            correlation,
        ));
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
        if self.preferences.open || self.workspace_dialog.open {
            return Task::none();
        }
        if let Some(delivery) = &mut self.delegation.events.delivering {
            if let DeliveryStage::ReadyAfterClear { input_revision } = delivery.stage
                && self.delegation.events.connected
            {
                delivery.stage = DeliveryStage::VerifyingAfterClear { input_revision };
                delivery.token = Uuid::new_v4();
                let token = delivery.token;
                let candidate = delivery.candidate.clone();
                return self.events_verify(token, candidate);
            }
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
        if !self.events_ready(&event, &candidate, false) {
            return Task::none();
        }
        let c = candidate.clone();
        let token = Uuid::new_v4();
        self.delegation.events.delivering = Some(Delivery {
            token,
            event,
            candidate,
            stage: DeliveryStage::Verifying,
        });
        self.events_verify(token, c)
    }

    fn events_verify(&self, token: Uuid, candidate: Candidate) -> Task<Message> {
        let epoch = self.delegation.epoch;
        let recall = self
            .delegation
            .events
            .delivering
            .as_ref()
            .filter(|d| d.event["amux_recall_done"] != true)
            .filter(|d| d.event["amux_guidance"].is_string() || assignment_task(&d.event).is_some())
            .map(|d| {
                let task = &d.event["payload"]["event"]["task"];
                format!(
                    "{} {} {} durable decisions and conventions",
                    candidate.target.project,
                    task["title"].as_str().unwrap_or_default(),
                    task["description"].as_str().unwrap_or_default()
                )
                .chars()
                .take(1000)
                .collect::<String>()
            });
        Task::perform(
            async move {
                let should_recall = recall.is_some();
                let (verified, memories) = tokio::task::spawn_blocking(move || {
                    let verified = verify(&candidate);
                    let memories = if verified {
                        recall.and_then(|query| {
                            amux::mcp::durable_memories(&candidate.target.directory, &query)
                        })
                    } else {
                        None
                    };
                    (verified, memories)
                })
                .await
                .unwrap_or_default();
                if should_recall {
                    Event::Recalled {
                        token,
                        verified,
                        memories,
                    }
                } else {
                    Event::Verified { token, verified }
                }
            },
            move |e| Message::Delegation(epoch, e),
        )
    }

    /// Check changing delivery conditions both before verification and just
    /// before writing to the terminal. Verification yields to other UI events.
    fn events_ready(&mut self, event: &Value, candidate: &Candidate, after_clear: bool) -> bool {
        if let Some(revision) = event["amux_input_revision"].as_u64()
            && tab(self, candidate.target.terminal)
                .is_none_or(|t| t.delegation.input_revision != revision)
        {
            self.events_result(
                event,
                "skipped",
                "user input changed before guidance delivery",
            );
            return false;
        }
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
        let offer = payload["mode"] == "assigned";
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
                || (!after_clear
                    && match (offer, output_at, reported) {
                        // The server judged this agent idle from the output time we
                        // reported; it is busy only if the terminal wrote since then.
                        (true, Some(output_at), Some(reported)) => output_at > reported + 1_000,
                        (true, None, _) => false,
                        _ => amux::session::output_active(elapsed),
                    })
        });
        if payload["require_idle"] == true && busy {
            // A work offer goes back so the server can offer it elsewhere;
            // an informational notice waits for the agent to go quiet.
            if offer || event["amux_guidance"] == "review" {
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
        if self
            .delegation
            .events
            .delivering
            .as_ref()
            .is_some_and(|delivery| {
                matches!(
                    delivery.stage,
                    DeliveryStage::Clearing { .. } | DeliveryStage::ReadyAfterClear { .. }
                )
            })
        {
            return Task::none();
        }
        if self.preferences.open
            || self.workspace_dialog.open
            || (!self.delegation.events.connected
                && self
                    .delegation
                    .events
                    .delivering
                    .as_ref()
                    .is_none_or(|d| !d.event["amux_guidance"].is_string()))
        {
            if let Some(delivery) = &mut self.delegation.events.delivering
                && let DeliveryStage::VerifyingAfterClear { input_revision } = delivery.stage
            {
                delivery.stage = DeliveryStage::ReadyAfterClear { input_revision };
            } else if let Some(delivery) = self.delegation.events.delivering.take() {
                self.delegation.events.pending.push_front(delivery.event);
            }
            return Task::none();
        }
        let Some(Delivery {
            mut event,
            candidate,
            stage,
            ..
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
        let input_revision = match stage {
            DeliveryStage::VerifyingAfterClear { input_revision } => Some(input_revision),
            _ => None,
        };
        if input_revision.is_some_and(|revision| {
            tab(self, current.target.terminal).is_none_or(|tab| {
                tab.delegation.input_revision != revision || !tab.delegation.input.is_empty()
            })
        }) {
            self.events_result(
                &event,
                "skipped",
                "agent input changed while clearing context",
            );
            return Task::none();
        }
        // /clear itself produces terminal output; only user input and execution
        // changes cancel the second stage, not that expected output.
        if !self.events_ready(&event, &current, input_revision.is_some()) {
            return Task::none();
        }
        let mcp = tab(self, current.target.terminal).is_some_and(|tab| tab.delegation.mcp_tools);
        let Some(mut message) = notification_text(&event, &current, mcp) else {
            self.events_result(&event, "skipped", "notification cannot be rendered");
            return Task::none();
        };
        // Persist cycle transitions and recall deduplication before any write.
        let registry = self.registry.clone();
        let restore = event["amux_guidance"] == "session"
            || event["amux_guidance"] == "clear"
            || input_revision.is_some();
        let memories = registry
            .as_deref()
            .ok_or_else(|| anyhow::anyhow!("session guidance storage unavailable"))
            .and_then(|registry| {
                amux::guidance::with_session(registry, current.target.session, |state| {
                    state.activate(current.actor.instance_id);
                    if event["amux_guidance"] == "review"
                        && event["amux_cycle"].as_str() != Some(state.cycle.to_string().as_str())
                    {
                        return Err("work cycle changed before review");
                    }
                    let new_assignment =
                        assignment_task(&event).is_some_and(|task| state.assign(task));
                    let needs_clear = assignment_task(&event).is_none_or(|task| {
                        (new_assignment || event["amux_assignment_new"] == true)
                            && state.needs_task_clear(task)
                    });
                    Ok((
                        event["amux_memories"]
                            .as_str()
                            .and_then(|text| state.memories(text, restore)),
                        needs_clear,
                        new_assignment,
                    ))
                })
            });
        let mut needs_clear = true;
        match memories {
            Ok(Ok((memories, clear, new_assignment))) => {
                needs_clear = clear;
                if new_assignment {
                    event["amux_assignment_new"] = true.into();
                }
                if let Some(memories) = memories {
                    message.push('\n');
                    message.push_str(&memories);
                }
            }
            Ok(Err(reason)) => {
                self.events_result(&event, "skipped", reason);
                return Task::none();
            }
            Err(error) => {
                if event["amux_guidance"].is_string() {
                    self.events_result(
                        &event,
                        "failed",
                        &format!("session guidance storage: {error:#}"),
                    );
                    return Task::none();
                }
                self.queue_diagnostic(format!(
                    "Task delivery continues without guidance bookkeeping: {error:#}"
                ));
            }
        }
        if event["amux_guidance"].is_string() || input_revision.is_some() {
            message.push('\n');
            let hints = if event["amux_guidance"] == "session" || event["amux_guidance"] == "clear"
            {
                amux::guidance::session_hints(mcp)
            } else if input_revision.is_some() {
                amux_gateway_backend::templates::SESSION_GUIDANCE.to_owned()
            } else {
                amux::guidance::capability_hints(mcp)
            };
            message.push_str(&hints);
        }
        if needs_clear && input_revision.is_none() && event["payload"]["clear_context"] == true {
            let ready = tab(self, current.target.terminal).is_some_and(|tab| {
                new_task_context(
                    &event,
                    tab.terminal.as_ref().and_then(Terminal::activity_elapsed),
                    !tab.delegation.input.is_empty(),
                )
            });
            if !ready {
                // The server and local idle clocks may differ slightly. Wait
                // rather than deliver a fallback without its required clear.
                self.delegation.events.pending.push_back(event);
                return Task::none();
            }
        }
        if input_revision.is_none()
            && needs_clear
            && (self.preferences.current.clear_on_task || event["payload"]["clear_context"] == true)
        {
            let clear_revision = tab(self, current.target.terminal).and_then(|tab| {
                new_task_context(
                    &event,
                    tab.terminal.as_ref().and_then(Terminal::activity_elapsed),
                    !tab.delegation.input.is_empty(),
                )
                .then_some(tab.delegation.input_revision)
            });
            if let Some(input_revision) = clear_revision {
                let reservation = registry
                    .as_deref()
                    .ok_or_else(|| anyhow::anyhow!("session guidance storage unavailable"))
                    .and_then(|registry| {
                        amux::guidance::with_session(registry, current.target.session, |state| {
                            if let Some(task) = assignment_task(&event) {
                                state.reserve_task_clear(task)
                            } else {
                                state.clear();
                                true
                            }
                        })
                    });
                if matches!(reservation, Ok(false)) {
                    self.events_result(&event, "skipped", "task context clear already reserved");
                    return Task::none();
                }
                if let Err(error) = reservation {
                    self.queue_diagnostic(format!(
                        "Task context clear continues without guidance bookkeeping: {error:#}"
                    ));
                }
                if !self.events_paste(&current, DeliveryInput::Clear, Some(input_revision)) {
                    self.events_result(&event, "failed", "terminal changed before context clear");
                    return Task::none();
                }
                let token = Uuid::new_v4();
                self.delegation.events.delivering = Some(Delivery {
                    token,
                    event,
                    candidate: current,
                    stage: DeliveryStage::Clearing {
                        started: Instant::now(),
                        input_revision,
                    },
                });
                let epoch = self.delegation.epoch;
                return Task::perform(
                    async {
                        tokio::time::sleep(CLEAR_WAIT).await;
                    },
                    move |_| Message::Delegation(epoch, Event::ClearReady { token }),
                );
            }
        }
        // Providers without bracketed paste interpret literal line breaks as
        // submissions. Contextual recall must remain one compact message.
        let message = amux::guidance::terminal_text(&message);
        let pasted = self.events_paste(
            &current,
            DeliveryInput::Notification(&message),
            input_revision,
        );
        if pasted {
            self.delegation
                .events
                .last_delivery
                .insert(current.target.session, Instant::now());
            self.events_result(&event, "delivered", "");
        } else {
            self.events_result(&event, "failed", "terminal changed before delivery");
        }
        Task::none()
    }

    fn events_paste(
        &mut self,
        candidate: &Candidate,
        input: DeliveryInput<'_>,
        input_revision: Option<u64>,
    ) -> bool {
        tab(self, candidate.target.terminal).is_some_and(|tab| {
            tab.delegation.root.as_ref()
                == Some(&(candidate.root.pid, candidate.root.generation.clone()))
                && input_revision.is_none_or(|revision| {
                    tab.delegation.input_revision == revision && tab.delegation.input.is_empty()
                })
                && tab.terminal.as_ref().is_some_and(|terminal| {
                    foreground(terminal) == candidate.root.foreground
                        && amux::identity::process_generation(candidate.process.pid)
                            .is_ok_and(|g| g == candidate.process.generation)
                        && match input {
                            // Slash commands must be typed, not bracketed-pasted
                            // as literal prompt text. End flushes provider paste bursts.
                            DeliveryInput::Clear => terminal
                                .try_enqueue_input(b"/clear\x1b[F\r".to_vec())
                                .is_ok(),
                            DeliveryInput::Notification(message) => {
                                paste(terminal, &tab.delegation.input, message)
                            }
                        }
                })
        })
    }

    pub(super) fn events_clear_ready(&mut self, token: Uuid) -> Task<Message> {
        let Some(delivery) = &mut self.delegation.events.delivering else {
            return Task::none();
        };
        if delivery.token != token {
            return Task::none();
        }
        if let DeliveryStage::Clearing {
            started,
            input_revision,
        } = delivery.stage
            && started.elapsed() >= CLEAR_WAIT
        {
            delivery.stage = DeliveryStage::ReadyAfterClear { input_revision };
            return self.events_deliver();
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
            .chain(self.delegation.events.guidance_cards.iter().rev().cloned())
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
pub(super) fn notification_text(event: &Value, candidate: &Candidate, mcp: bool) -> Option<String> {
    if let Some(kind) = event["amux_guidance"].as_str() {
        return Some(if kind == "review" {
            amux_gateway_backend::templates::MEMORY_REVIEW.to_owned()
        } else {
            "[Amux context restored] Current session guidance and durable recall follow.".to_owned()
        });
    }
    let payload = &event["payload"];
    let mut source = payload["event"].clone();
    source["id"] = event["seq"].clone();
    let mut parsed = delegation::Event::parse(&source).ok()??;
    if let (Some(heading), Some(summary)) = (payload["heading"].as_str(), parsed.summary.as_mut()) {
        summary.heading = heading.into();
    }
    let text = if payload["mode"] == "assigned" {
        parsed.assigned_terminal_text(&candidate.name)
    } else {
        parsed.recipient_terminal_text(&candidate.target, &candidate.name)
    };
    let text = match payload["instructions"].as_str() {
        Some(instructions) => format!("{text} {instructions}"),
        None => text,
    };
    Some(format!(
        "{text} {}{}",
        if assignment_task(event).is_some() {
            amux_gateway_backend::templates::TASK_GUIDANCE
        } else {
            amux_gateway_backend::templates::TASK_UPDATE_GUIDANCE
        },
        if assignment_task(event).is_some() {
            amux::guidance::capability_hints(mcp)
        } else {
            String::new()
        }
    ))
}

fn assignment_task(event: &Value) -> Option<Uuid> {
    let payload = &event["payload"];
    ((payload["new_task"] == true || payload["mode"] == "assigned")
        && payload["event"]["kind"] == "task_created"
        && payload["event"]["comment"].is_null())
    .then(|| {
        payload["event"]["task"]["id"]
            .as_str()
            .and_then(|id| Uuid::parse_str(id).ok())
    })
    .flatten()
}

#[cfg(test)]
mod clear_tests {
    use super::*;

    #[test]
    fn clear_requires_explicit_new_work_and_an_idle_empty_composer() {
        let event = json!({"payload":{"new_task":true,"event":{"kind":"task_created","comment":null,"task":{"id":Uuid::new_v4(),"labels":[]}}}});
        let idle = Some(amux::session::IDLE_AFTER);
        assert!(new_task_context(&event, idle, false));
        assert!(!new_task_context(
            &event,
            Some(Duration::from_secs(29)),
            false
        ));
        assert!(!new_task_context(&event, None, false));
        assert!(!new_task_context(&event, idle, true));
        for kind in ["task_commented", "task_completed"] {
            let mut notice = event.clone();
            notice["payload"]["event"]["kind"] = json!(kind);
            assert!(!new_task_context(&notice, idle, false));
        }
        for value in [Value::Null, json!(false)] {
            let mut notice = event.clone();
            notice["payload"]["new_task"] = value;
            assert!(!new_task_context(&notice, idle, false));
        }
        let mut review = event.clone();
        review["payload"]["event"]["task"]["labels"] = json!(["task-review"]);
        assert!(!new_task_context(&review, idle, false));
        let mut comment = event;
        comment["payload"]["event"]["comment"] = json!({"content":"continue"});
        assert!(!new_task_context(&comment, idle, false));
    }
}
