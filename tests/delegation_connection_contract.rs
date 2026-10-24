//! Reconnecting lifecycle subscription contract: subscribed validation, compact events,
//! recorded receipts, heartbeats, stale-command discard, redaction and cancellation.
//! Local WebSocket fixtures only.
use amux::delegation::Kind;
use amux::delegation_connection::{Command, ConnectionEvent, run};
use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use std::{
    sync::{Arc, Mutex},
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::{net::TcpListener, sync::mpsc, task::JoinHandle};
use tokio_tungstenite::tungstenite::{
    Message,
    handshake::server::{Request, Response},
};
use uuid::Uuid;

const KEY: &str = "secret-test-key-91c2";

type Server = tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>;

struct Harness {
    listener: TcpListener,
    consumer: Uuid,
    commands: mpsc::Sender<Command>,
    events: mpsc::Receiver<ConnectionEvent>,
    worker: JoinHandle<()>,
    seen: Vec<String>,
}

async fn start(cursor: Option<i64>) -> Harness {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let consumer = Uuid::new_v4();
    let (commands, command_rx) = mpsc::channel(8);
    let (event_tx, events) = mpsc::channel(8);
    let worker = tokio::spawn(run(url, KEY.into(), consumer, cursor, command_rx, event_tx));
    Harness {
        listener,
        consumer,
        commands,
        events,
        worker,
        seen: Vec::new(),
    }
}

impl Harness {
    /// Accept one upgrade, require the Bearer key and stream path, and return the subscribe frame.
    // The handshake callback's error type is fixed by tungstenite.
    #[allow(clippy::result_large_err)]
    async fn accept(&self) -> (Server, Value) {
        let (tcp, _) = timeout(self.listener.accept()).await.unwrap();
        let seen = Arc::new(Mutex::new((String::new(), String::new())));
        let record = seen.clone();
        let mut ws =
            tokio_tungstenite::accept_hdr_async(tcp, move |req: &Request, res: Response| {
                let auth = req
                    .headers()
                    .get("authorization")
                    .map(|v| v.to_str().unwrap().to_owned());
                *record.lock().unwrap() = (req.uri().path().to_owned(), auth.unwrap_or_default());
                Ok(res)
            })
            .await
            .unwrap();
        let (path, auth) = seen.lock().unwrap().clone();
        assert_eq!(path, "/v1/tasks/stream");
        assert_eq!(auth, format!("Bearer {KEY}"));
        let subscribe = read_text(&mut ws).await;
        (ws, subscribe)
    }

    async fn next(&mut self) -> ConnectionEvent {
        let event = timeout(self.events.recv()).await.expect("worker stopped");
        let text = format!("{event:?}");
        assert!(!text.contains(KEY) && !text.contains("PRIVATE"), "{text}");
        self.seen.push(text);
        event
    }

    async fn state(&mut self) -> String {
        match self.next().await {
            ConnectionEvent::State(state) => state,
            other => panic!("expected state, got {other:?}"),
        }
    }

    /// Answer the subscribe frame and expect the Connected/Subscribed pair.
    async fn subscribe(&mut self, ws: &mut Server, cursor: i64, latest: i64) -> u64 {
        send(ws, self.subscribed(cursor, latest)).await;
        assert_eq!(self.state().await, "Connected");
        match self.next().await {
            ConnectionEvent::Subscribed {
                connection,
                cursor: c,
                replay_through,
            } => {
                assert_eq!((c, replay_through), (cursor, latest));
                connection
            }
            other => panic!("expected subscribed, got {other:?}"),
        }
    }

    fn subscribed(&self, cursor: i64, latest: i64) -> Value {
        json!({"type":"subscribed","protocol_version":1,"consumer_id":self.consumer,
            "cursor":cursor,"latest_event_id":latest})
    }

    async fn command(&self, connection: u64, frame: Value) {
        self.commands
            .send(Command { connection, frame })
            .await
            .unwrap();
    }
}

async fn timeout<F: std::future::Future>(future: F) -> F::Output {
    tokio::time::timeout(Duration::from_secs(15), future)
        .await
        .expect("timed out")
}

async fn send(ws: &mut Server, value: Value) {
    ws.send(Message::Text(value.to_string())).await.unwrap();
}

async fn read_text(ws: &mut Server) -> Value {
    loop {
        match timeout(ws.next()).await.unwrap().unwrap() {
            Message::Text(text) => return serde_json::from_str(&text).unwrap(),
            Message::Pong(_) | Message::Ping(_) => {}
            other => panic!("unexpected client frame {other:?}"),
        }
    }
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}

fn lifecycle(id: i64, task: Uuid) -> Value {
    json!({"type":"event","event":{"id":id,"project_ident":"github.com/owner/repo",
        "kind":"task_created","created_at":now(),
        "task":{"id":task,"title":"PRIVATE title","description":format!("PRIVATE {KEY}")}}})
}

fn ack(id: i64) -> Value {
    json!({"type":"ack","event_id":id,"status":"injected"})
}

#[tokio::test]
async fn subscribed_events_receipts_heartbeat_and_scoped_commands() {
    let mut h = start(Some(41)).await;
    assert_eq!(h.state().await, "Connecting");
    let (mut ws, subscribe) = h.accept().await;
    assert_eq!(
        subscribe,
        json!({"type":"subscribe","protocol_version":1,"consumer_id":h.consumer,"after_event_id":41})
    );
    let connection = h.subscribe(&mut ws, 41, 45).await;

    // Backlog at or below replay_through is still reported; the caller records the skip.
    let task = Uuid::new_v4();
    send(&mut ws, lifecycle(44, task)).await;
    match h.next().await {
        ConnectionEvent::Event(event) => {
            assert_eq!(
                (event.id, event.task, event.kind),
                (44, task, Kind::Created)
            );
            assert_eq!(event.project, "github.com/owner/repo");
        }
        other => panic!("expected event, got {other:?}"),
    }

    // Delegated source notifications and invalid payloads keep their ID for a receipt, nothing else.
    send(&mut ws, json!({"type":"event","event":{"id":46,"project_ident":"p","kind":"task_created",
        "created_at":now(),"task":{"id":Uuid::new_v4(),"kind":"delegated","delegated_to_task_id":"x"}}})).await;
    send(&mut ws, json!({"type":"event","event":{"id":47,"kind":"task_deleted","task":{"description":"PRIVATE"}}})).await;
    for id in [46, 47] {
        match h.next().await {
            ConnectionEvent::Rejected { event_id, .. } => assert_eq!(event_id, id),
            other => panic!("expected rejected, got {other:?}"),
        }
    }

    // Only acks for the current connection are forwarded, unchanged.
    h.command(connection + 1, ack(44)).await;
    h.command(connection, json!({"type":"subscribe","after_event_id":0}))
        .await;
    h.command(connection, ack(44)).await;
    assert_eq!(read_text(&mut ws).await, ack(44));

    send(
        &mut ws,
        json!({"type":"recorded","event_id":44,"status":"injected"}),
    )
    .await;
    send(
        &mut ws,
        json!({"type":"recorded","event_id":47,"status":"failed"}),
    )
    .await;
    for (id, status, cursor) in [(44, "injected", 44), (47, "failed", 47)] {
        match h.next().await {
            ConnectionEvent::Recorded {
                event_id,
                status: s,
                cursor: c,
            } => assert_eq!((event_id, s.as_str(), c), (id, status, cursor)),
            other => panic!("expected recorded, got {other:?}"),
        }
    }

    send(&mut ws, json!({"type":"heartbeat"})).await;
    assert_eq!(read_text(&mut ws).await, json!({"type":"heartbeat"}));
    send(&mut ws, json!({"type":"heartbeat_ack"})).await;

    // Unknown receipt status is a protocol error, reported without payload.
    send(
        &mut ws,
        json!({"type":"recorded","event_id":48,"status":"PRIVATE"}),
    )
    .await;
    assert_eq!(h.state().await, "Invalid delivery receipt");
    drop(h.events);
    timeout(h.worker).await.unwrap();
}

#[tokio::test]
async fn reconnect_resumes_cursor_and_discards_stale_commands() {
    let mut h = start(None).await;
    assert_eq!(h.state().await, "Connecting");
    let (mut ws, subscribe) = h.accept().await;
    assert_eq!(subscribe["after_event_id"], Value::Null);
    let first = h.subscribe(&mut ws, 10, 12).await;
    send(
        &mut ws,
        json!({"type":"recorded","event_id":13,"status":"received"}),
    )
    .await;
    assert!(matches!(
        h.next().await,
        ConnectionEvent::Recorded { cursor: 13, .. }
    ));
    ws.close(None).await.unwrap();
    drop(ws);
    assert_eq!(h.state().await, "Gateway stream closed");

    // Commands queued across the gap belong to no live subscription and are never sent.
    h.command(first, ack(13)).await;
    h.command(first + 1, ack(12)).await;
    assert_eq!(h.state().await, "Connecting");
    let (mut ws, subscribe) = h.accept().await;
    assert_eq!(subscribe["after_event_id"], 13);
    h.command(first + 1, ack(11)).await;

    // A regressing tail on the new connection is rejected before any event is accepted.
    send(&mut ws, h.subscribed(13, 11)).await;
    assert_eq!(h.state().await, "Invalid subscription tail");
    drop(ws);
    assert_eq!(h.state().await, "Connecting");
    let (mut ws, subscribe) = h.accept().await;
    assert_eq!(subscribe["after_event_id"], 13);
    let second = h.subscribe(&mut ws, 13, 20).await;
    assert!(second > first);

    h.command(first, ack(13)).await;
    h.command(second, ack(15)).await;
    assert_eq!(read_text(&mut ws).await, ack(15));
    drop(h.commands);
    timeout(h.worker).await.unwrap();
}

#[tokio::test]
async fn malformed_subscription_is_rejected() {
    let base = |h: &Harness| h.subscribed(5, 9);
    type Case = (fn(&mut Value), &'static str);
    let cases: [Case; 7] = [
        (
            |v| v["protocol_version"] = json!(2),
            "Gateway subscription mismatch",
        ),
        (
            |v| v["consumer_id"] = json!(Uuid::new_v4()),
            "Gateway subscription mismatch",
        ),
        (|v| v["cursor"] = json!(4), "Gateway subscription mismatch"),
        (|v| v["cursor"] = json!(-1), "Invalid subscribed cursor"),
        (
            |v| v["latest_event_id"] = json!(4),
            "Invalid subscription tail",
        ),
        (
            |v| *v = lifecycle(6, Uuid::nil()),
            "Unexpected gateway protocol message",
        ),
        (
            |v| *v = json!({"type":"error","message":KEY}),
            "Gateway rejected the subscription",
        ),
    ];
    for (mutate, expected) in cases {
        let mut h = start(Some(5)).await;
        assert_eq!(h.state().await, "Connecting");
        let (mut ws, _) = h.accept().await;
        let mut frame = base(&h);
        mutate(&mut frame);
        send(&mut ws, frame).await;
        assert_eq!(h.state().await, expected);
        drop(h.events);
        timeout(h.worker).await.unwrap();
    }
}

#[tokio::test]
async fn rejected_upgrade_and_errors_are_redacted() {
    let mut h = start(None).await;
    assert_eq!(h.state().await, "Connecting");
    let (mut tcp, _) = timeout(h.listener.accept()).await.unwrap();
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let mut buffer = [0; 4096];
    let n = tcp.read(&mut buffer).await.unwrap();
    assert!(String::from_utf8_lossy(&buffer[..n]).contains(KEY));
    tcp.write_all(
        format!(
            "HTTP/1.1 401 Unauthorized\r\nContent-Length: {}\r\n\r\n{KEY}",
            KEY.len()
        )
        .as_bytes(),
    )
    .await
    .unwrap();
    assert_eq!(h.state().await, "Gateway upgrade rejected or unavailable");

    assert_eq!(h.state().await, "Connecting");
    let (mut ws, _) = h.accept().await;
    h.subscribe(&mut ws, 0, 0).await;
    send(
        &mut ws,
        json!({"type":"error","message":format!("bad key {KEY}")}),
    )
    .await;
    assert_eq!(
        h.state().await,
        "Gateway rejected the subscription or delivery receipt"
    );
    assert!(h.seen.iter().all(|s| !s.contains(KEY)));
    drop(h.commands);
    timeout(h.worker).await.unwrap();
}

#[tokio::test]
async fn cancellation_while_connecting_backing_off_and_subscribed() {
    // Unreachable port: the worker backs off and must still stop when the caller leaves.
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    drop(listener);
    let (commands, command_rx) = mpsc::channel(1);
    let (event_tx, mut events) = mpsc::channel(1);
    let worker = tokio::spawn(run(
        url,
        KEY.into(),
        Uuid::new_v4(),
        None,
        command_rx,
        event_tx,
    ));
    assert!(matches!(
        timeout(events.recv()).await,
        Some(ConnectionEvent::State(_))
    ));
    assert!(matches!(
        timeout(events.recv()).await,
        Some(ConnectionEvent::State(_))
    ));
    drop(commands);
    timeout(worker).await.unwrap();

    // Accepted but never subscribed: dropping the events receiver ends the wait.
    let mut h = start(None).await;
    assert_eq!(h.state().await, "Connecting");
    let (_ws, _) = h.accept().await;
    drop(h.events);
    timeout(h.worker).await.unwrap();

    // Subscribed: dropping the command sender ends the connection.
    let mut h = start(None).await;
    assert_eq!(h.state().await, "Connecting");
    let (mut ws, _) = h.accept().await;
    h.subscribe(&mut ws, 0, 0).await;
    drop(h.commands);
    timeout(h.worker).await.unwrap();
    assert!(matches!(
        ws.next().await,
        None | Some(Ok(Message::Close(_))) | Some(Err(_))
    ));
}
