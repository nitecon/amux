//! Reconnecting client for the server event router (`/v1/events/stream`).
//! It owns only the socket: the caller handles events and sends acks and
//! emits through `outgoing`. Frames queued while disconnected are dropped;
//! the server resends any event that was not acked.
use crate::gateway_stream::{GatewayClock, NO_EVENT_ROUTER, connect_events, next_json, send_json};
use serde_json::{Value, json};
use std::time::Duration;
use tokio::{sync::mpsc, time::Instant};
use uuid::Uuid;

const HEARTBEAT: Duration = Duration::from_secs(20);
const SILENCE: Duration = Duration::from_secs(60);
const RECONNECT: Duration = Duration::from_secs(5);
/// A gateway without the router is checked again rarely.
const UNSUPPORTED_RETRY: Duration = Duration::from_secs(300);

#[derive(Debug, Clone)]
pub enum Incoming {
    /// Subscribed; times sent to the server should use its clock.
    Connected(Option<GatewayClock>),
    Event(Value),
    /// The server rejected a frame, e.g. an invalid emit.
    Rejected(String),
    /// The gateway has no event router (a legacy gateway).
    Unsupported,
    Lost(String),
}

#[derive(Debug, Clone)]
pub struct Subscription {
    pub consumer: Uuid,
    pub events: Vec<String>,
    pub targets: Vec<String>,
}

pub fn ack(event: &Value) -> Value {
    json!({"type": "ack", "seq": event["seq"]})
}

pub fn emit(id: &str, payload: Value, correlation: Option<&str>) -> Value {
    json!({"type": "emit", "id": id, "version": 1, "payload": payload, "correlation": correlation})
}

/// Run until `incoming` or `outgoing` closes.
pub async fn run(
    profile: crate::gateway_transport::TransportProfile,
    subscription: Subscription,
    mut outgoing: mpsc::Receiver<Value>,
    incoming: mpsc::Sender<Incoming>,
) {
    loop {
        let wait = match session(&profile, &subscription, &mut outgoing, &incoming).await {
            Ok(()) => return,
            Err(error) if error == NO_EVENT_ROUTER => {
                if incoming.send(Incoming::Unsupported).await.is_err() {
                    return;
                }
                UNSUPPORTED_RETRY
            }
            Err(error) => {
                if incoming.send(Incoming::Lost(error)).await.is_err() {
                    return;
                }
                RECONNECT
            }
        };
        let resume = tokio::time::sleep(wait);
        tokio::pin!(resume);
        loop {
            tokio::select! {
                _ = &mut resume => break,
                frame = outgoing.recv() => if frame.is_none() { return },
                _ = incoming.closed() => return,
            }
        }
    }
}

async fn session(
    profile: &crate::gateway_transport::TransportProfile,
    subscription: &Subscription,
    outgoing: &mut mpsc::Receiver<Value>,
    incoming: &mpsc::Sender<Incoming>,
) -> Result<(), String> {
    let (mut socket, clock) = connect_events(profile).await?;
    send_json(
        &mut socket,
        json!({"type": "subscribe", "consumer_id": subscription.consumer,
               "events": subscription.events, "targets": subscription.targets}),
    )
    .await?;
    let reply = tokio::time::timeout(Duration::from_secs(10), next_json(&mut socket))
        .await
        .map_err(|_| "Event subscription timed out")??;
    if reply["type"] != "subscribed" {
        return Err("Event subscription rejected".into());
    }
    if incoming.send(Incoming::Connected(clock)).await.is_err() {
        return Ok(());
    }
    let mut heartbeat = tokio::time::interval_at(Instant::now() + HEARTBEAT, HEARTBEAT);
    let mut deadline = Instant::now() + SILENCE;
    loop {
        tokio::select! {
            frame = next_json(&mut socket) => {
                let frame = frame?;
                deadline = Instant::now() + SILENCE;
                let message = match frame["type"].as_str() {
                    Some("event") => Incoming::Event(frame["event"].clone()),
                    Some("error") => Incoming::Rejected(
                        frame["message"].as_str().unwrap_or("rejected").chars().take(256).collect(),
                    ),
                    _ => continue,
                };
                if incoming.send(message).await.is_err() {
                    return Ok(());
                }
            }
            frame = outgoing.recv() => match frame {
                Some(frame) => send_json(&mut socket, frame).await?,
                None => return Ok(()),
            },
            _ = heartbeat.tick() => send_json(&mut socket, json!({"type": "heartbeat"})).await?,
            _ = tokio::time::sleep_until(deadline) => return Err("Event stream went silent".into()),
            _ = incoming.closed() => return Ok(()),
        }
    }
}
