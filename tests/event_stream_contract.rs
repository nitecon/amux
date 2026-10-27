//! Real-server contract for router events: task mutations become `task.*`
//! events, and remote consumers receive them over `/v1/events/stream`.
use amux_gateway_backend::{
    db,
    runtime::{self, RuntimeConfig},
};
use futures_util::{SinkExt, StreamExt};
use serde_json::{Value, json};
use std::time::Duration;
use tokio::net::TcpListener;
use tokio_tungstenite::{
    MaybeTlsStream, WebSocketStream, connect_async,
    tungstenite::{Message, client::IntoClientRequest},
};
use uuid::Uuid;

const KEY: &str = "event-stream-fixture-key";
const PROJECT: &str = "event-stream-fixture";
type Socket = WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;

struct Server {
    endpoint: String,
    http: reqwest::Client,
    _root: tempfile::TempDir,
    _server: tokio::task::JoinHandle<()>,
}

impl Server {
    async fn start() -> Self {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("events.sqlite");
        let config = RuntimeConfig {
            listen_addr: "127.0.0.1:1".parse().unwrap(),
            database: db::DatabaseConfig::sqlite(path.to_string_lossy()),
            api_key: KEY.into(),
            default_channel: "discord".into(),
            retention_days: 30,
            bot_retention_days: 7,
            artifact_operations: db::ArtifactOperationsEnvelope::default(),
            artifact_body_schema_enabled: true,
            artifact_auth_enforced: false,
            ui_auth_enabled: true,
            discord: None,
            whatsapp: None,
        };
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("127.0.0.1:{}", listener.local_addr().unwrap().port());
        let server = tokio::spawn(async move { runtime::serve(config, listener).await.unwrap() });
        let http = reqwest::Client::new();
        let server = Self {
            endpoint,
            http,
            _root: root,
            _server: server,
        };
        let registered = server
            .send(
                reqwest::Method::POST,
                "/v1/projects",
                json!({"ident": PROJECT, "channel": "discord"}),
            )
            .await;
        assert_eq!(registered["ident"], PROJECT);
        server
    }

    async fn send(&self, method: reqwest::Method, path: &str, body: Value) -> Value {
        let response = self
            .http
            .request(method, format!("http://{}{path}", self.endpoint))
            .bearer_auth(KEY)
            .header("X-Agent-Session-Id", "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001")
            .header(
                "X-Agent-Instance-Id",
                "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0002",
            )
            .header("X-Agent-Provider", "claude")
            .header("X-Agent-OS", "linux")
            .json(&body)
            .send()
            .await
            .unwrap();
        assert!(
            response.status().is_success(),
            "{path}: {}",
            response.status()
        );
        response.json().await.unwrap()
    }

    async fn socket(&self, key: &str) -> Result<Socket, tokio_tungstenite::tungstenite::Error> {
        let mut request = format!("ws://{}/v1/events/stream", self.endpoint)
            .into_client_request()
            .unwrap();
        request
            .headers_mut()
            .insert("authorization", format!("Bearer {key}").parse().unwrap());
        Ok(connect_async(request).await?.0)
    }

    async fn subscribe(&self, consumer: Uuid, events: &[&str], after: Option<i64>) -> Socket {
        let mut socket = self.socket(KEY).await.unwrap();
        send(
            &mut socket,
            json!({"type": "subscribe", "consumer_id": consumer, "events": events, "after": after}),
        )
        .await;
        assert_eq!(frame(&mut socket).await["type"], "subscribed");
        socket
    }
}

async fn send(socket: &mut Socket, value: Value) {
    socket.send(Message::Text(value.to_string())).await.unwrap();
}

/// The next non-heartbeat frame.
async fn frame(socket: &mut Socket) -> Value {
    loop {
        let message = tokio::time::timeout(Duration::from_secs(10), socket.next())
            .await
            .expect("no frame from the event stream")
            .unwrap()
            .unwrap();
        if let Message::Text(text) = message {
            let value: Value = serde_json::from_str(&text).unwrap();
            if value["type"] != "heartbeat" {
                return value;
            }
        }
    }
}

async fn event(socket: &mut Socket, ack: bool) -> Value {
    let value = frame(socket).await;
    assert_eq!(value["type"], "event", "{value}");
    let event = value["event"].clone();
    if ack {
        send(socket, json!({"type": "ack", "seq": event["seq"]})).await;
        loop {
            let reply = frame(socket).await;
            // The server may resend the same event before it reads the ack.
            if reply["type"] == "event" && reply["event"]["seq"] == event["seq"] {
                continue;
            }
            assert_eq!(reply, json!({"type": "acked", "seq": event["seq"]}));
            break;
        }
    }
    event
}

#[tokio::test]
async fn task_mutations_reach_only_subscribed_remote_consumers_at_least_once() {
    let server = Server::start().await;
    assert!(server.socket("wrong-key").await.is_err());
    let lifecycle = Uuid::new_v4();
    let comments = Uuid::new_v4();
    let mut a = server
        .subscribe(
            lifecycle,
            &["task.created", "task.started", "task.completed"],
            None,
        )
        .await;
    let mut b = server.subscribe(comments, &["task.commented"], None).await;

    let task = server
        .send(
            reqwest::Method::POST,
            &format!("/v1/projects/{PROJECT}/tasks"),
            json!({"title": "Routed task"}),
        )
        .await;
    let id = task["id"].as_str().unwrap().to_owned();
    let path = format!("/v1/projects/{PROJECT}/tasks/{id}");
    server
        .send(
            reqwest::Method::PATCH,
            &path,
            json!({"status": "in_progress"}),
        )
        .await;
    server
        .send(
            reqwest::Method::POST,
            &format!("{path}/comments"),
            json!({"content": "progress"}),
        )
        .await;
    server
        .send(reqwest::Method::PATCH, &path, json!({"status": "done"}))
        .await;

    let created = event(&mut a, true).await;
    assert_eq!(created["id"], "task.created");
    assert_eq!(created["source"], "tasks");
    assert_eq!(created["payload"]["project"], PROJECT);
    assert_eq!(created["payload"]["task"]["id"], id);
    assert_eq!(created["payload"]["actor"]["provider"], "claude");
    let started = event(&mut a, true).await;
    assert_eq!(started["id"], "task.started");
    assert_eq!(started["payload"]["task"]["status"], "in_progress");
    // Unacked events are resent after the backoff, then delivered once acked.
    let completed = event(&mut a, false).await;
    assert_eq!(completed["id"], "task.completed");
    let resent = event(&mut a, true).await;
    assert_eq!(resent["seq"], completed["seq"]);

    let commented = event(&mut b, true).await;
    assert_eq!(commented["id"], "task.commented");
    assert_eq!(commented["payload"]["comment"]["content"], "progress");

    // Nothing else is queued for either consumer: no duplicates, no other kinds.
    send(&mut a, json!({"type": "heartbeat"})).await;
    assert_eq!(frame(&mut a).await, json!({"type": "heartbeat_ack"}));
    send(&mut b, json!({"type": "heartbeat"})).await;
    assert_eq!(frame(&mut b).await, json!({"type": "heartbeat_ack"}));

    // A reconnecting consumer resumes after its last ack.
    drop(a);
    let mut a = server
        .subscribe(
            lifecycle,
            &["task.created", "task.started", "task.completed"],
            None,
        )
        .await;
    server
        .send(
            reqwest::Method::POST,
            &format!("/v1/projects/{PROJECT}/tasks"),
            json!({"title": "Second"}),
        )
        .await;
    let second = event(&mut a, true).await;
    assert_eq!(second["id"], "task.created");
    assert_eq!(second["payload"]["task"]["title"], "Second");

    // A new consumer may replay from an explicit cursor.
    let mut replay = server
        .subscribe(Uuid::new_v4(), &["task.created"], Some(0))
        .await;
    assert_eq!(
        event(&mut replay, true).await["payload"]["task"]["title"],
        "Routed task"
    );
    assert_eq!(
        event(&mut replay, true).await["payload"]["task"]["title"],
        "Second"
    );
}

#[tokio::test]
async fn clients_emit_only_declared_remote_events_with_valid_payloads() {
    let server = Server::start().await;
    let mut socket = server
        .subscribe(Uuid::new_v4(), &["task.created"], None)
        .await;
    let activity = json!({
        "session_id": "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001",
        "instance_id": "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0002",
        "output_at": 1, "draft": false,
    });
    send(
        &mut socket,
        json!({"type": "emit", "id": "terminal.activity", "version": 1, "payload": activity}),
    )
    .await;
    let emitted = frame(&mut socket).await;
    assert_eq!(emitted["type"], "emitted", "{emitted}");
    assert!(Uuid::parse_str(emitted["uid"].as_str().unwrap()).is_ok());
    send(&mut socket, json!({"type": "emit", "id": "terminal.activity", "version": 1, "payload": {"draft": false}})).await;
    assert_eq!(frame(&mut socket).await["type"], "error");
    send(
        &mut socket,
        json!({"type": "emit", "id": "task.created", "version": 1, "payload": {}}),
    )
    .await;
    let rejected = frame(&mut socket).await;
    assert!(
        rejected["message"]
            .as_str()
            .unwrap()
            .contains("cannot be emitted by clients")
    );
    let mut other = server.socket(KEY).await.unwrap();
    send(
        &mut other,
        json!({"type": "subscribe", "consumer_id": Uuid::new_v4(), "events": ["clock.tick"]}),
    )
    .await;
    assert!(
        frame(&mut other).await["message"]
            .as_str()
            .unwrap()
            .contains("not forwarded")
    );
}

#[tokio::test]
async fn heartbeats_require_each_agents_identity_and_project() {
    let server = Server::start().await;
    let mut socket = server
        .subscribe(Uuid::new_v4(), &["task.created"], None)
        .await;
    let agent = json!({
        "session_id": "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001",
        "instance_id": "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0002",
        "provider": "claude", "os": "linux", "project": PROJECT, "name": "Fixture",
    });
    send(&mut socket, json!({"type": "emit", "id": "client.heartbeat", "version": 1, "payload": {"agents": [agent]}})).await;
    assert_eq!(frame(&mut socket).await["type"], "emitted");
    let mut unscoped = agent.clone();
    unscoped.as_object_mut().unwrap().remove("project");
    send(&mut socket, json!({"type": "emit", "id": "client.heartbeat", "version": 1, "payload": {"agents": [unscoped]}})).await;
    assert_eq!(frame(&mut socket).await["type"], "error");
}
