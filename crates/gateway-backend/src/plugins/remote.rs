//! Remote consumers. Clients subscribe to forwarded events over
//! `/v1/events/stream`, receive them one at a time until acked (resent with the
//! router's backoff), and may emit events declared `remote_emit`.
use crate::{
    db,
    router::{
        self, Bus, Event, EventDefinition, Plugin, Step, StepContext, StepOutcome, Subscription,
    },
    routes::AppError,
    AppState,
};
use anyhow::{bail, Result};
use async_trait::async_trait;
use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        State,
    },
    http::{HeaderMap, StatusCode},
    response::Response,
};
use rusqlite::{params, Connection, OptionalExtension};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{
    collections::HashSet,
    sync::{Arc, OnceLock},
    time::Duration,
};
use tokio::{sync::Notify, time::Instant};

pub const PLUGIN: &str = "remote";
pub const HEARTBEAT: &str = "client.heartbeat";
pub const ACTIVITY: &str = "terminal.activity";
const FRAME_BYTES: usize = 65536;
const SCAN: i64 = 100;

/// Shared between the forwarding plugin and stream connections.
#[derive(Clone, Default)]
pub struct Remote(Arc<Shared>);

#[derive(Default)]
struct Shared {
    bus: OnceLock<Bus>,
    forwarded: OnceLock<HashSet<&'static str>>,
    wake: Notify,
}

impl Remote {
    pub fn attach(&self, bus: Bus) {
        let _ = self.0.bus.set(bus);
    }

    /// Whether the event router runs behind this server.
    pub fn attached(&self) -> bool {
        self.0.bus.get().is_some()
    }

    /// The plugin that stores and announces every forwarded event.
    pub fn plugin(&self, forwarded: Vec<&'static str>) -> Box<dyn Plugin> {
        let _ = self.0.forwarded.set(forwarded.iter().copied().collect());
        Box::new(Forward {
            remote: self.clone(),
            forwarded,
        })
    }
}

struct Forward {
    remote: Remote,
    forwarded: Vec<&'static str>,
}

impl Plugin for Forward {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        self.forwarded
            .iter()
            .map(|event| Subscription {
                event,
                versions: 1..=u32::MAX,
                depends_on: Vec::new(),
            })
            .collect()
    }
    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Announce(self.remote.clone()))]
    }
}

/// Consumers read stored events by cursor; this only wakes their connections.
struct Announce(Remote);

#[async_trait]
impl Step for Announce {
    async fn run(&self, _event: &Event, _ctx: &StepContext) -> StepOutcome {
        self.0 .0.wake.notify_waiters();
        StepOutcome::Continue
    }
}

/// Events clients report; consumed by server plugins.
pub fn events() -> Vec<EventDefinition> {
    vec![
        EventDefinition {
            id: HEARTBEAT,
            version: 1,
            remote_emit: true,
            validate: heartbeat,
        },
        EventDefinition {
            id: ACTIVITY,
            version: 1,
            remote_emit: true,
            validate: activity,
        },
    ]
}

/// Every agent the client currently runs:
/// `{"agents":[{"session_id","instance_id","provider","os","project",
///   "name"?,"goal"?,"agent_id"?,"generation"?}]}`
fn heartbeat(payload: &Value) -> std::result::Result<(), String> {
    let agents = payload["agents"]
        .as_array()
        .ok_or("agents must be a list")?;
    for agent in agents {
        for field in ["session_id", "instance_id", "provider", "os", "project"] {
            if !agent[field].is_string() {
                return Err(format!("agent {field} must be a string"));
            }
        }
        for field in ["name", "goal", "agent_id", "generation"] {
            if !(agent[field].is_null() || agent[field].is_string()) {
                return Err(format!("agent {field} must be a string when present"));
            }
        }
    }
    Ok(())
}

/// `{"session_id","instance_id","output_at":ms|null,"draft":bool}`
fn activity(payload: &Value) -> std::result::Result<(), String> {
    if !payload["session_id"].is_string() || !payload["instance_id"].is_string() {
        return Err("session_id and instance_id must be strings".into());
    }
    if !(payload["output_at"].is_null() || payload["output_at"].is_i64()) {
        return Err("output_at must be milliseconds or null".into());
    }
    if !payload["draft"].is_boolean() {
        return Err("draft must be a boolean".into());
    }
    Ok(())
}

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS remote_consumers (
            consumer_id   TEXT PRIMARY KEY,
            connection_id TEXT,
            cursor        INTEGER NOT NULL,
            last_seen_at  INTEGER NOT NULL
        );",
    )?;
    Ok(())
}

#[derive(Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum Input {
    /// A new consumer starts after the newest stored event unless `after` is
    /// given; a known consumer resumes from its acked cursor.
    Subscribe {
        consumer_id: String,
        events: Vec<String>,
        #[serde(default)]
        targets: Vec<String>,
        after: Option<i64>,
    },
    Ack {
        seq: i64,
    },
    Emit {
        id: String,
        version: u32,
        payload: Value,
        /// The event uid this one answers, e.g. a delivery result.
        correlation: Option<String>,
    },
    Heartbeat,
}

struct Consumer {
    id: String,
    events: HashSet<String>,
    targets: HashSet<String>,
}

struct Inflight {
    seq: i64,
    sent: Instant,
    attempts: i64,
    frame: Value,
}

pub async fn connect(
    State(state): State<AppState>,
    headers: HeaderMap,
    peer: Option<axum::Extension<crate::secure::Principal>>,
    ws: WebSocketUpgrade,
) -> std::result::Result<Response, AppError> {
    let bearer = headers
        .get("authorization")
        .and_then(|h| h.to_str().ok())
        .and_then(|h| h.strip_prefix("Bearer "));
    if peer.is_none() && (state.api_key.is_empty() || bearer != Some(state.api_key.as_str())) {
        return Err(AppError(
            StatusCode::UNAUTHORIZED,
            "stream bearer token required".into(),
        ));
    }
    if state.remote.0.bus.get().is_none() {
        return Err(AppError(
            StatusCode::SERVICE_UNAVAILABLE,
            "event router is not running".into(),
        ));
    }
    Ok(ws
        .max_message_size(FRAME_BYTES)
        .max_frame_size(FRAME_BYTES)
        .on_upgrade(move |socket| serve(socket, state, peer.map(|p| p.0))))
}

async fn send(socket: &mut WebSocket, value: &Value) -> Result<()> {
    tokio::time::timeout(
        Duration::from_secs(5),
        socket.send(Message::Text(value.to_string().into())),
    )
    .await??;
    Ok(())
}

async fn serve(mut socket: WebSocket, state: AppState, peer: Option<crate::secure::Principal>) {
    let remote = state.remote.clone();
    let bus = remote.0.bus.get().unwrap().clone();
    let source = peer.as_ref().map_or("client:api-key".to_owned(), |peer| {
        format!("client:{}", peer.node.node_id)
    });
    let connection = uuid::Uuid::now_v7().to_string();
    let mut consumer: Option<Consumer> = None;
    let mut cursor = 0;
    let mut inflight: Option<Inflight> = None;
    let mut last_seen = Instant::now();
    let mut heartbeat = Instant::now();
    let mut tick = tokio::time::interval(Duration::from_millis(250));
    loop {
        let reply: Option<Result<Value>> = tokio::select! {
            message = socket.recv() => {
                let Some(Ok(message)) = message else { break };
                if matches!(message, Message::Close(_)) { break }
                let Message::Text(text) = message else { continue };
                last_seen = Instant::now();
                Some(handle(&text, &bus, &remote, &peer, &source, &connection, &mut consumer, &mut cursor, &mut inflight).await)
            }
            _ = remote.0.wake.notified() => None,
            _ = tick.tick() => None,
        };
        if let Some(reply) = reply {
            let frame = reply
                .unwrap_or_else(|error| json!({"type": "error", "message": error.to_string()}));
            if send(&mut socket, &frame).await.is_err() {
                break;
            }
        }
        if peer.as_ref().is_some_and(|peer| !peer.valid())
            || last_seen.elapsed() > Duration::from_secs(60)
        {
            break;
        }
        let Some(subscribed) = &consumer else {
            continue;
        };
        let db = bus.db().clone();
        let owned = owns(&db.lock().unwrap(), &connection, &subscribed.id);
        if !owned.unwrap_or(false) {
            break;
        }
        match &mut inflight {
            Some(pending) => {
                if pending.sent.elapsed()
                    >= Duration::from_millis(router::retry_delay(pending.attempts) as u64)
                {
                    pending.attempts += 1;
                    pending.sent = Instant::now();
                    if send(&mut socket, &pending.frame).await.is_err() {
                        break;
                    }
                }
            }
            None => {
                let next = next(&db.lock().unwrap(), subscribed, &mut cursor);
                match next {
                    Ok(Some(event)) => {
                        let frame = json!({"type": "event", "event": frame(&event)});
                        if send(&mut socket, &frame).await.is_err() {
                            break;
                        }
                        inflight = Some(Inflight {
                            seq: event.seq,
                            sent: Instant::now(),
                            attempts: 1,
                            frame,
                        });
                    }
                    Ok(None) => {}
                    Err(error) => {
                        tracing::warn!("Event stream read failed: {error}");
                        break;
                    }
                }
            }
        }
        if heartbeat.elapsed() > Duration::from_secs(20) {
            if send(&mut socket, &json!({"type": "heartbeat"}))
                .await
                .is_err()
            {
                break;
            }
            heartbeat = Instant::now();
        }
    }
    if let Some(consumer) = consumer {
        let _ = bus.db().lock().unwrap().execute(
            "UPDATE remote_consumers SET connection_id=NULL WHERE consumer_id=?1 AND connection_id=?2",
            params![consumer.id, connection],
        );
    }
}

#[allow(clippy::too_many_arguments)]
async fn handle(
    text: &str,
    bus: &Bus,
    remote: &Remote,
    peer: &Option<crate::secure::Principal>,
    source: &str,
    connection: &str,
    consumer: &mut Option<Consumer>,
    cursor: &mut i64,
    inflight: &mut Option<Inflight>,
) -> Result<Value> {
    match serde_json::from_str(text)? {
        Input::Subscribe {
            consumer_id,
            events,
            targets,
            after,
        } => {
            if consumer.is_some() {
                bail!("already subscribed");
            }
            uuid::Uuid::parse_str(&consumer_id)
                .map_err(|_| anyhow::anyhow!("consumer_id must be a UUID"))?;
            let forwarded = remote.0.forwarded.get().cloned().unwrap_or_default();
            if let Some(unknown) = events
                .iter()
                .find(|event| !forwarded.contains(event.as_str()))
            {
                bail!("event {unknown} is not forwarded to remote consumers");
            }
            if let Some(peer) = peer {
                peer.store.bind_consumer(&peer.node.node_id, &consumer_id)?;
            }
            *cursor = subscribe(&bus.db().lock().unwrap(), connection, &consumer_id, after)?;
            *consumer = Some(Consumer {
                id: consumer_id.clone(),
                events: events.into_iter().collect(),
                targets: targets.into_iter().collect(),
            });
            Ok(json!({"type": "subscribed", "consumer_id": consumer_id, "cursor": *cursor}))
        }
        Input::Ack { seq } => {
            let subscribed = consumer
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("subscribe first"))?;
            match inflight {
                Some(pending) if pending.seq == seq => {
                    *cursor = seq;
                    *inflight = None;
                    advance(&bus.db().lock().unwrap(), &subscribed.id, seq)?;
                    Ok(json!({"type": "acked", "seq": seq}))
                }
                _ if seq <= *cursor => Ok(json!({"type": "acked", "seq": seq})),
                _ => bail!("event {seq} is not in flight"),
            }
        }
        Input::Emit {
            id,
            version,
            payload,
            correlation,
        } => {
            if !bus.accepts_remote(&id, version) {
                bail!("event {id} v{version} cannot be emitted by clients");
            }
            let correlation = correlation
                .map(|uid| uuid::Uuid::parse_str(&uid))
                .transpose()
                .map_err(|_| anyhow::anyhow!("correlation must be an event uid"))?;
            let uid = bus.emit(source, &id, version, payload, correlation).await?;
            Ok(json!({"type": "emitted", "uid": uid.to_string()}))
        }
        Input::Heartbeat => Ok(json!({"type": "heartbeat_ack"})),
    }
}

fn frame(event: &Event) -> Value {
    json!({
        "seq": event.seq, "id": event.id, "version": event.version, "uid": event.uid.to_string(),
        "payload": event.payload, "source": event.source, "emitted_at": event.emitted_at,
        "correlation": event.correlation.map(|c| c.to_string()),
    })
}

fn subscribe(
    conn: &Connection,
    connection: &str,
    consumer: &str,
    after: Option<i64>,
) -> Result<i64> {
    let known: Option<i64> = conn
        .query_row(
            "SELECT cursor FROM remote_consumers WHERE consumer_id=?1",
            params![consumer],
            |row| row.get(0),
        )
        .optional()?;
    let tail: i64 = conn.query_row(
        "SELECT COALESCE(MAX(seq),0) FROM router_events",
        [],
        |row| row.get(0),
    )?;
    let cursor = after.or(known).unwrap_or(tail);
    conn.execute(
        "INSERT INTO remote_consumers(consumer_id,connection_id,cursor,last_seen_at) VALUES(?1,?2,?3,?4)
         ON CONFLICT(consumer_id) DO UPDATE SET connection_id=?2, cursor=?3, last_seen_at=?4",
        params![consumer, connection, cursor, db::now_ms()],
    )?;
    Ok(cursor)
}

/// A newer connection for the same consumer takes over.
fn owns(conn: &Connection, connection: &str, consumer: &str) -> Result<bool> {
    let owner: Option<String> = conn
        .query_row(
            "SELECT connection_id FROM remote_consumers WHERE consumer_id=?1",
            params![consumer],
            |row| row.get(0),
        )
        .optional()?
        .flatten();
    Ok(owner.as_deref() == Some(connection))
}

fn advance(conn: &Connection, consumer: &str, seq: i64) -> Result<()> {
    conn.execute(
        "UPDATE remote_consumers SET cursor=MAX(cursor,?2), last_seen_at=?3 WHERE consumer_id=?1",
        params![consumer, seq, db::now_ms()],
    )?;
    Ok(())
}

/// The next event for this consumer after `cursor`. Events it does not
/// subscribe to, or that target another agent, move the cursor past them.
fn next(conn: &Connection, consumer: &Consumer, cursor: &mut i64) -> Result<Option<Event>> {
    loop {
        let seqs = conn
            .prepare("SELECT seq FROM router_events WHERE seq > ?1 ORDER BY seq LIMIT ?2")?
            .query_map(params![*cursor, SCAN], |row| row.get::<_, i64>(0))?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        let Some(&last) = seqs.last() else {
            return Ok(None);
        };
        for seq in seqs {
            let event = router::load(conn, seq)?;
            let targeted = event.payload["target"]
                .as_str()
                .is_none_or(|target| consumer.targets.contains(target));
            if consumer.events.contains(&event.id) && targeted {
                return Ok(Some(event));
            }
            *cursor = seq;
        }
        advance(conn, &consumer.id, last)?;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn consumers_skip_unsubscribed_and_other_agents_targeted_events() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        let rows = [
            ("task.created", json!({})),
            ("agent.notify", json!({"target": "other"})),
            ("agent.notify", json!({"target": "mine"})),
            ("agent.notify", json!({})),
        ];
        for (seq, (id, payload)) in rows.iter().enumerate() {
            conn.execute(
                "INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at)
                 VALUES(?1,?2,?3,1,?4,'test',0)",
                params![
                    seq as i64 + 1,
                    uuid::Uuid::now_v7().to_string(),
                    id,
                    payload.to_string()
                ],
            )
            .unwrap();
        }
        subscribe(&conn, "connection", "consumer", Some(0)).unwrap();
        let consumer = Consumer {
            id: "consumer".into(),
            events: ["agent.notify".to_owned()].into(),
            targets: ["mine".to_owned()].into(),
        };
        let mut cursor = 0;
        let first = next(&conn, &consumer, &mut cursor).unwrap().unwrap();
        assert_eq!((first.seq, cursor), (3, 2));
        cursor = 3;
        let untargeted = next(&conn, &consumer, &mut cursor).unwrap().unwrap();
        assert_eq!(untargeted.seq, 4);
        cursor = 4;
        assert!(next(&conn, &consumer, &mut cursor).unwrap().is_none());
        assert!(owns(&conn, "connection", "consumer").unwrap());
        subscribe(&conn, "newer", "consumer", None).unwrap();
        assert!(
            !owns(&conn, "connection", "consumer").unwrap(),
            "a newer connection takes over"
        );
    }
}
