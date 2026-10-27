//! Reconnecting gateway lifecycle subscription (CMUX protocol v1 parity).
//! Owns only the socket: delivery state, receipts and the durable cursor belong to the caller.
//! State text is static and never contains the API key, raw frames or network errors.
use crate::{
    delegation::Event,
    gateway_stream::{GatewayClock, Socket, connect_profile_generation, next_json, send_json},
};
use serde_json::{Value, json};
use std::{
    sync::atomic::{AtomicU64, Ordering},
    time::Duration,
};
use tokio::{sync::mpsc, time::Instant};
use uuid::Uuid;

const SUBSCRIBE_DEADLINE: Duration = Duration::from_secs(10);
const MAX_SILENCE: Duration = Duration::from_secs(30);
const HEARTBEAT: Duration = Duration::from_secs(10);
const RECONNECT: Duration = Duration::from_secs(5);
const CONNECT_DEADLINE: Duration = Duration::from_secs(35);
static NEXT_CONNECTION: AtomicU64 = AtomicU64::new(1);
const STATUSES: [&str; 6] = [
    "received",
    "queued",
    "injected",
    "skipped",
    "failed",
    "uncertain",
];

/// One outgoing frame, valid only on the connection whose `Subscribed` it answers.
#[derive(Debug, Clone)]
pub struct Command {
    pub connection: u64,
    pub frame: Value,
}

#[derive(Debug, Clone)]
pub enum ConnectionEvent {
    State(String),
    /// Events with `id <= replay_through` are offline backlog, never live input.
    Subscribed {
        connection: u64,
        cursor: i64,
        replay_through: i64,
    },
    /// Gateway time from the upgrade `Date`, sent after `Subscribed` and before any event.
    Clock(GatewayClock),
    Event(Box<Event>),
    /// A positive event ID with no deliverable compact event; the caller still owns its receipt.
    Rejected {
        event_id: i64,
        reason: String,
    },
    Recorded {
        event_id: i64,
        status: String,
        cursor: i64,
    },
}

/// Subscribe after `cursor`, reconnecting every five seconds until `events` closes or `commands` drops.
/// Commands from an earlier connection, or queued while unsubscribed, are discarded, never resent.
/// Callers must not block on `commands` while they stop draining `events`.
pub async fn run(
    url: String,
    key: String,
    consumer: Uuid,
    cursor: Option<i64>,
    commands: mpsc::Receiver<Command>,
    events: mpsc::Sender<ConnectionEvent>,
) {
    match crate::gateway_transport::TransportProfile::legacy(&url, key) {
        Ok(profile) => run_profile(profile, consumer, cursor, commands, events).await,
        Err(_) => {
            let _ = events
                .send(ConnectionEvent::State("Invalid gateway connection".into()))
                .await;
        }
    }
}

pub async fn run_profile(
    profile: crate::gateway_transport::TransportProfile,
    consumer: Uuid,
    mut cursor: Option<i64>,
    mut commands: mpsc::Receiver<Command>,
    events: mpsc::Sender<ConnectionEvent>,
) {
    let mut latest = 0;
    loop {
        if events
            .send(ConnectionEvent::State("Connecting".into()))
            .await
            .is_err()
        {
            return;
        }
        let attempt = tokio::select! {
            socket = tokio::time::timeout(CONNECT_DEADLINE, connect_profile_generation(&profile, consumer, cursor)) =>
                socket.unwrap_or_else(|_| Err("Gateway connection timed out; reconnecting".into())),
            _ = discard(&mut commands) => return,
            _ = events.closed() => return,
        };
        let error = match attempt {
            Ok((socket, clock, generation)) => {
                let connection = NEXT_CONNECTION.fetch_add(1, Ordering::Relaxed);
                let mut stream = Stream {
                    socket,
                    clock,
                    consumer,
                    connection,
                    cursor: &mut cursor,
                    latest: &mut latest,
                    commands: &mut commands,
                    events: &events,
                    profile: &profile,
                    generation,
                };
                match stream.run().await {
                    Ok(()) => return,
                    Err(error) => error,
                }
            }
            Err(error) => error,
        };
        if events.send(ConnectionEvent::State(error)).await.is_err() {
            return;
        }
        tokio::select! {
            _ = tokio::time::sleep(RECONNECT) => {}
            _ = discard(&mut commands) => return,
            _ = events.closed() => return,
        }
    }
}

/// Discard commands until the sender drops; nothing is sent while unsubscribed.
async fn discard(commands: &mut mpsc::Receiver<Command>) {
    while commands.recv().await.is_some() {}
}

/// One socket lifetime. `Ok` stops the worker; `Err` carries static text and reconnects.
struct Stream<'a> {
    socket: Socket,
    clock: Option<GatewayClock>,
    consumer: Uuid,
    connection: u64,
    cursor: &'a mut Option<i64>,
    latest: &'a mut i64,
    commands: &'a mut mpsc::Receiver<Command>,
    events: &'a mpsc::Sender<ConnectionEvent>,
    profile: &'a crate::gateway_transport::TransportProfile,
    generation: String,
}

enum Step {
    Frame(Result<Value, String>),
    Command(Command),
    Heartbeat,
}

impl Stream<'_> {
    async fn run(&mut self) -> Result<(), String> {
        let subscribed = tokio::select! {
            frame = next_json(&mut self.socket) => frame?,
            _ = tokio::time::sleep(SUBSCRIBE_DEADLINE) => return Err("Gateway subscription timed out".into()),
            _ = discard(self.commands) => return Ok(()),
            _ = self.events.closed() => return Ok(()),
        };
        let (cursor, latest) = self.subscribed(&subscribed)?;
        *self.cursor = Some(cursor);
        *self.latest = latest;
        while self.commands.try_recv().is_ok() {}
        if !self.emit(ConnectionEvent::State("Connected".into())).await
            || !self
                .emit(ConnectionEvent::Subscribed {
                    connection: self.connection,
                    cursor,
                    replay_through: latest,
                })
                .await
        {
            return Ok(());
        }
        if let Some(clock) = self.clock.take()
            && !self.emit(ConnectionEvent::Clock(clock)).await
        {
            return Ok(());
        }
        let mut heartbeat = tokio::time::interval_at(Instant::now() + HEARTBEAT, HEARTBEAT);
        heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        let mut silence = Instant::now() + MAX_SILENCE;
        loop {
            let step = tokio::select! {
                frame = next_json(&mut self.socket) => Step::Frame(frame),
                command = self.commands.recv() => match command {
                    Some(command) => Step::Command(command),
                    None => return Ok(()),
                },
                _ = heartbeat.tick() => Step::Heartbeat,
                _ = tokio::time::sleep_until(silence) => return Err("Gateway stream timed out; reconnecting".into()),
                _ = self.events.closed() => return Ok(()),
            };
            match step {
                Step::Frame(frame) => {
                    silence = Instant::now() + MAX_SILENCE;
                    let Some(event) = self.incoming(frame?).await? else {
                        continue;
                    };
                    if !self.emit(event).await {
                        return Ok(());
                    }
                }
                // Root receipts only; the worker keeps no ACK state and never resends.
                Step::Command(command) => {
                    if command.connection == self.connection && command.frame["type"] == "ack" {
                        send_json(&mut self.socket, command.frame).await?;
                    }
                }
                Step::Heartbeat => {
                    if Instant::now() >= silence {
                        return Err("Gateway stream timed out; reconnecting".into());
                    }
                    if self.profile.is_enrolled() {
                        let snapshot = tokio::time::timeout_at(silence, self.profile.current())
                            .await
                            .map_err(|_| "Gateway stream timed out; reconnecting")?
                            .map_err(|_| "Gateway credential renewal failed")?;
                        if snapshot.generation != self.generation {
                            return Err("Gateway credentials renewed; reconnecting".into());
                        }
                    }
                    send_json(&mut self.socket, json!({"type":"heartbeat"})).await?;
                }
            }
        }
    }

    /// Validate the first server frame against this consumer and every cursor already observed.
    fn subscribed(&self, value: &Value) -> Result<(i64, i64), String> {
        if value["type"] == "error" {
            return Err("Gateway rejected the subscription".into());
        }
        if value["type"] != "subscribed" {
            return Err("Unexpected gateway protocol message".into());
        }
        let cursor = value["cursor"]
            .as_i64()
            .filter(|c| *c >= 0)
            .ok_or("Invalid subscribed cursor")?;
        let latest = value["latest_event_id"]
            .as_i64()
            .filter(|id| *id >= cursor)
            .ok_or("Invalid subscription tail")?;
        if value["protocol_version"] != 1
            || value["consumer_id"].as_str() != Some(self.consumer.to_string().as_str())
            || self.cursor.is_some_and(|known| cursor < known)
            || latest < *self.latest
        {
            return Err("Gateway subscription mismatch".into());
        }
        Ok((cursor, latest))
    }

    /// Map one subscribed-phase frame; heartbeats are answered here and produce no event.
    async fn incoming(&mut self, value: Value) -> Result<Option<ConnectionEvent>, String> {
        match value["type"].as_str() {
            Some("event") => {
                let event_id = value["event"]["id"]
                    .as_i64()
                    .filter(|id| *id > 0)
                    .ok_or("Invalid lifecycle event ID")?;
                Ok(Some(match Event::parse(&value["event"]) {
                    Ok(Some(event)) => ConnectionEvent::Event(Box::new(event)),
                    Ok(None) => ConnectionEvent::Rejected {
                        event_id,
                        reason: "Delegated source task notification".into(),
                    },
                    Err(_) => ConnectionEvent::Rejected {
                        event_id,
                        reason: "Unsupported or invalid lifecycle event".into(),
                    },
                }))
            }
            Some("recorded") => {
                let event_id = value["event_id"]
                    .as_i64()
                    .filter(|id| *id > 0)
                    .ok_or("Invalid receipt ID")?;
                let status = value["status"]
                    .as_str()
                    .filter(|s| STATUSES.contains(s))
                    .ok_or("Invalid delivery receipt")?;
                let cursor = self.cursor.unwrap_or(0).max(event_id);
                *self.cursor = Some(cursor);
                Ok(Some(ConnectionEvent::Recorded {
                    event_id,
                    status: status.into(),
                    cursor,
                }))
            }
            Some("heartbeat") => {
                send_json(&mut self.socket, json!({"type":"heartbeat"})).await?;
                Ok(None)
            }
            Some("heartbeat_ack") => Ok(None),
            Some("error") => Err("Gateway rejected the subscription or delivery receipt".into()),
            _ => Err("Unexpected gateway protocol message".into()),
        }
    }

    async fn emit(&self, event: ConnectionEvent) -> bool {
        self.events.send(event).await.is_ok()
    }
}
