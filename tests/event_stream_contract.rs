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
        self.subscribe_targets(consumer, events, &[], after).await
    }

    async fn subscribe_targets(
        &self,
        consumer: Uuid,
        events: &[&str],
        targets: &[&str],
        after: Option<i64>,
    ) -> Socket {
        let mut socket = self.socket(KEY).await.unwrap();
        send(
            &mut socket,
            json!({"type": "subscribe", "consumer_id": consumer, "events": events,
                   "targets": targets, "after": after}),
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

const SESSION: &str = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001";
const INSTANCE: &str = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0002";

fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}

/// A desktop stand-in that runs one agent and receives its notifications.
async fn desktop(server: &Server) -> Socket {
    let mut socket = server
        .subscribe_targets(
            Uuid::new_v4(),
            &["agent.notify", "agent.idle", "agent.busy"],
            &[SESSION],
            None,
        )
        .await;
    let agent = json!({
        "session_id": SESSION, "instance_id": INSTANCE, "provider": "claude",
        "os": "linux", "project": PROJECT, "name": "Fixture",
    });
    emit(
        &mut socket,
        "client.heartbeat",
        json!({"agents": [agent]}),
        None,
    )
    .await;
    socket
}

async fn emit(socket: &mut Socket, id: &str, payload: Value, correlation: Option<&Value>) {
    send(
        socket,
        json!({"type": "emit", "id": id, "version": 1, "payload": payload, "correlation": correlation}),
    )
    .await;
    loop {
        let reply = frame(socket).await;
        if reply["type"] == "emitted" {
            return;
        }
        assert_eq!(reply["type"], "event", "{reply}");
        // Leave events for `expect`; they are resent until acked.
    }
}

async fn activity(socket: &mut Socket, output_at: i64, draft: bool) {
    let payload = json!({"session_id": SESSION, "instance_id": INSTANCE, "output_at": output_at, "draft": draft});
    emit(socket, "terminal.activity", payload, None).await;
}

/// Ack events until one with `id` arrives.
async fn expect(socket: &mut Socket, id: &str) -> Value {
    loop {
        let event = event(socket, true).await;
        if event["id"] == id {
            return event;
        }
    }
}

async fn task(server: &Server, id: &str) -> Value {
    let response = server
        .http
        .get(format!(
            "http://{}/v1/projects/{PROJECT}/tasks/{id}",
            server.endpoint
        ))
        .bearer_auth(KEY)
        .send()
        .await
        .unwrap();
    response.json().await.unwrap()
}

#[tokio::test]
async fn idle_agents_are_assigned_the_oldest_task_and_undelivered_offers_are_released() {
    let server = Server::start().await;
    let mut socket = desktop(&server).await;
    // A draft keeps an otherwise quiet agent busy, so nothing is assigned.
    activity(&mut socket, now_ms() - 60_000, true).await;
    expect(&mut socket, "agent.busy").await;
    let first = server
        .send(
            reqwest::Method::POST,
            &format!("/v1/projects/{PROJECT}/tasks"),
            json!({"title": "First"}),
        )
        .await;
    let second = server
        .send(
            reqwest::Method::POST,
            &format!("/v1/projects/{PROJECT}/tasks"),
            json!({"title": "Second"}),
        )
        .await;
    let first = first["id"].as_str().unwrap().to_owned();
    let second = second["id"].as_str().unwrap().to_owned();
    tokio::time::sleep(Duration::from_millis(500)).await;
    assert_eq!(task(&server, &first).await["status"], "todo");

    // Quiet and no draft: idle, so it is offered the oldest task.
    activity(&mut socket, now_ms() - 60_000, false).await;
    expect(&mut socket, "agent.idle").await;
    let offer = expect(&mut socket, "agent.notify").await;
    assert_eq!(offer["payload"]["target"], SESSION);
    assert_eq!(offer["payload"]["mode"], "assigned");
    assert_eq!(offer["payload"]["require_idle"], true);
    assert_eq!(offer["payload"]["heading"], "Queued task review");
    assert_eq!(offer["payload"]["event"]["task"]["id"], first);
    let claimed = task(&server, &first).await;
    assert_eq!(claimed["status"], "in_progress");
    assert_eq!(claimed["owner_origin"]["session_id"], SESSION);
    assert_eq!(
        task(&server, &second).await["status"],
        "todo",
        "one task per agent"
    );

    // The client could not deliver it: the claim is released.
    let result = json!({"target": SESSION, "status": "skipped", "reason": "agent became busy"});
    emit(
        &mut socket,
        "agent.notify_result",
        result,
        Some(&offer["uid"]),
    )
    .await;
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    while task(&server, &first).await["status"] != "todo" {
        assert!(
            tokio::time::Instant::now() < deadline,
            "skipped offer was not released"
        );
        tokio::time::sleep(Duration::from_millis(50)).await;
    }

    // Another idle agent is offered the released task first.
    const OTHER: &str = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0003";
    let mut other = server
        .subscribe_targets(Uuid::new_v4(), &["agent.notify"], &[OTHER], None)
        .await;
    let agents = json!({"agents": [
        {"session_id": SESSION, "instance_id": INSTANCE, "provider": "claude", "os": "linux", "project": PROJECT},
        {"session_id": OTHER, "instance_id": INSTANCE, "provider": "claude", "os": "linux", "project": PROJECT},
    ]});
    emit(&mut socket, "client.heartbeat", agents, None).await;
    let quiet = json!({"session_id": OTHER, "instance_id": INSTANCE, "output_at": now_ms() - 60_000, "draft": false});
    emit(&mut socket, "terminal.activity", quiet, None).await;
    let reoffer = expect(&mut other, "agent.notify").await;
    assert_eq!(reoffer["payload"]["target"], OTHER);
    assert_eq!(reoffer["payload"]["event"]["task"]["id"], first);
    let delivered = json!({"target": OTHER, "status": "delivered", "reason": null});
    emit(
        &mut other,
        "agent.notify_result",
        delivered,
        Some(&reoffer["uid"]),
    )
    .await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    let taken = task(&server, &first).await;
    assert_eq!(
        taken["status"], "in_progress",
        "a delivered offer keeps its claim"
    );
    assert_eq!(taken["owner_origin"]["session_id"], OTHER);
}

#[tokio::test]
async fn owners_are_told_about_comments_on_their_tasks_but_not_their_own() {
    let server = Server::start().await;
    let mut socket = desktop(&server).await;
    activity(&mut socket, now_ms() - 60_000, false).await;
    let offer = expect(&mut socket, "agent.idle").await;
    assert_eq!(offer["payload"]["agent"]["session_id"], SESSION);
    let created = server
        .send(
            reqwest::Method::POST,
            &format!("/v1/projects/{PROJECT}/tasks"),
            json!({"title": "Owned"}),
        )
        .await;
    let id = created["id"].as_str().unwrap();
    expect(&mut socket, "agent.notify").await;
    // The agent's own comment (same origin headers) is not echoed back.
    server
        .send(
            reqwest::Method::POST,
            &format!("/v1/projects/{PROJECT}/tasks/{id}/comments"),
            json!({"content": "mine"}),
        )
        .await;
    // A user's comment reaches the owner.
    let response = server
        .http
        .post(format!(
            "http://{}/v1/projects/{PROJECT}/tasks/{id}/comments",
            server.endpoint
        ))
        .bearer_auth(KEY)
        .json(&json!({"content": "please also check docs"}))
        .send()
        .await
        .unwrap();
    assert!(response.status().is_success());
    let told = expect(&mut socket, "agent.notify").await;
    assert_eq!(told["payload"]["mode"], "recipient");
    assert_eq!(told["payload"]["event"]["kind"], "task_commented");
    assert_eq!(
        told["payload"]["event"]["comment"]["content"],
        "please also check docs"
    );
}

#[tokio::test]
async fn the_desktop_event_client_reports_agents_and_receives_their_work() {
    use amux::event_client::{self, Incoming, Subscription};
    let server = Server::start().await;
    let profile = amux::gateway_transport::TransportProfile::legacy(
        &format!("http://{}", server.endpoint),
        KEY.into(),
    )
    .unwrap();
    let (outgoing, outgoing_rx) = tokio::sync::mpsc::channel(16);
    let (incoming_tx, mut incoming) = tokio::sync::mpsc::channel(16);
    let subscription = Subscription {
        consumer: Uuid::new_v4(),
        events: vec!["agent.notify".into(), "agent.idle".into()],
        targets: vec![SESSION.into()],
    };
    tokio::spawn(event_client::run(
        profile,
        subscription,
        outgoing_rx,
        incoming_tx,
    ));
    let Incoming::Connected(clock) = next_incoming(&mut incoming).await else {
        panic!("not connected");
    };
    assert!(
        clock.is_some(),
        "server time comes from the upgrade response"
    );
    let agent = json!({"session_id": SESSION, "instance_id": INSTANCE, "provider": "claude", "os": "linux", "project": PROJECT});
    outgoing
        .send(event_client::emit(
            "client.heartbeat",
            json!({"agents": [agent]}),
            None,
        ))
        .await
        .unwrap();
    let quiet = json!({"session_id": SESSION, "instance_id": INSTANCE, "output_at": now_ms() - 60_000, "draft": false});
    outgoing
        .send(event_client::emit("terminal.activity", quiet, None))
        .await
        .unwrap();
    let Incoming::Event(idle) = next_incoming(&mut incoming).await else {
        panic!("expected an event");
    };
    assert_eq!(idle["id"], "agent.idle");
    outgoing.send(event_client::ack(&idle)).await.unwrap();
    server
        .send(
            reqwest::Method::POST,
            &format!("/v1/projects/{PROJECT}/tasks"),
            json!({"title": "Work"}),
        )
        .await;
    let Incoming::Event(notify) = next_incoming(&mut incoming).await else {
        panic!("expected a notification");
    };
    assert_eq!(notify["id"], "agent.notify");
    assert_eq!(notify["payload"]["target"], SESSION);
    outgoing.send(event_client::ack(&notify)).await.unwrap();
    // An invalid emit is reported back without dropping the connection.
    outgoing
        .send(event_client::emit("terminal.activity", json!({}), None))
        .await
        .unwrap();
    assert!(matches!(
        next_incoming(&mut incoming).await,
        Incoming::Rejected(_)
    ));
}

#[tokio::test]
async fn a_gateway_without_the_event_router_is_reported_as_unsupported() {
    use amux::event_client::{self, Incoming, Subscription};
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let endpoint = format!("http://127.0.0.1:{}", listener.local_addr().unwrap().port());
    let legacy = axum::Router::new().fallback(|| async { axum::http::StatusCode::NOT_FOUND });
    tokio::spawn(async move { axum::serve(listener, legacy).await.unwrap() });
    let profile = amux::gateway_transport::TransportProfile::legacy(&endpoint, KEY.into()).unwrap();
    let (_outgoing, outgoing_rx) = tokio::sync::mpsc::channel(1);
    let (incoming_tx, mut incoming) = tokio::sync::mpsc::channel(4);
    let subscription = Subscription {
        consumer: Uuid::new_v4(),
        events: Vec::new(),
        targets: Vec::new(),
    };
    tokio::spawn(event_client::run(
        profile,
        subscription,
        outgoing_rx,
        incoming_tx,
    ));
    let first = tokio::time::timeout(Duration::from_secs(10), incoming.recv())
        .await
        .unwrap()
        .unwrap();
    assert!(matches!(first, Incoming::Unsupported), "{first:?}");
}

async fn next_incoming(
    incoming: &mut tokio::sync::mpsc::Receiver<amux::event_client::Incoming>,
) -> amux::event_client::Incoming {
    tokio::time::timeout(Duration::from_secs(10), incoming.recv())
        .await
        .expect("no message from the event client")
        .unwrap()
}
