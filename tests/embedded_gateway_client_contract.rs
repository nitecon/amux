//! Native amux clients talking to the imported gateway router and a real SQLite
//! file. The deterministic channel is a test fixture, never a production adapter.
use amux::{
    gateway::Gateway,
    gateway_stream::{self, Socket},
    identity::{AgentIdentity, Provider},
    task_routing::SessionTarget,
};
use amux_gateway_backend::{
    AppState, api_router,
    channel::{ChannelPlugin, InboundMessage, OutboundMessage, PluginEvent},
    db::{self, Db},
};
use async_trait::async_trait;
use reqwest::Method;
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    path::{Path, PathBuf},
    process::Command,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};
use tokio::{net::TcpListener, sync::mpsc};
use uuid::Uuid;

const KEY: &str = "isolated-embedded-client-fixture-key";
const PROJECT: &str = "native-client-fixture";
const REMOTE: &str = "github.com/fixtures/native-client-fixture";

#[derive(Debug, Clone, PartialEq, Eq)]
enum ChannelCall {
    EnsureRoom(String),
    RegisterRoom(String, Option<String>),
    Structured {
        room: String,
        agent: String,
        subject: String,
        body: String,
        hostname: String,
        event_at: i64,
    },
    ReplyStructured {
        room: String,
        parent_external: String,
        agent: String,
        subject: String,
        body: String,
        hostname: String,
        event_at: i64,
    },
    Raw {
        room: String,
        content: String,
    },
    RawReply {
        room: String,
        parent_external: String,
        content: String,
    },
}

#[derive(Default)]
struct FixtureChannel {
    calls: Mutex<Vec<ChannelCall>>,
    sequence: AtomicUsize,
    next_error: Mutex<Option<String>>,
}

impl FixtureChannel {
    fn outcome(&self) -> anyhow::Result<String> {
        if let Some(error) = self.next_error.lock().unwrap().take() {
            anyhow::bail!(error);
        }
        let sequence = self.sequence.fetch_add(1, Ordering::SeqCst) + 1;
        Ok(format!("fixture-external-message-{sequence}"))
    }

    fn fail_next(&self, error: &str) {
        *self.next_error.lock().unwrap() = Some(error.into());
    }
}

#[async_trait]
impl ChannelPlugin for FixtureChannel {
    fn name(&self) -> &str {
        "fixture"
    }

    fn register_room(&self, room: &str, last: Option<&str>) {
        self.calls.lock().unwrap().push(ChannelCall::RegisterRoom(
            room.into(),
            last.map(str::to_owned),
        ));
    }

    async fn start(&self, _tx: mpsc::Sender<PluginEvent>) -> anyhow::Result<()> {
        Ok(())
    }

    async fn ensure_room(&self, project: &str) -> anyhow::Result<String> {
        self.calls
            .lock()
            .unwrap()
            .push(ChannelCall::EnsureRoom(project.into()));
        let room = format!("fixture-room-{project}");
        self.register_room(&room, None);
        Ok(room)
    }

    async fn send(&self, room: &str, content: &str) -> anyhow::Result<String> {
        self.calls.lock().unwrap().push(ChannelCall::Raw {
            room: room.into(),
            content: content.into(),
        });
        self.outcome()
    }

    async fn reply(&self, room: &str, parent: &str, content: &str) -> anyhow::Result<String> {
        self.calls.lock().unwrap().push(ChannelCall::RawReply {
            room: room.into(),
            parent_external: parent.into(),
            content: content.into(),
        });
        self.outcome()
    }

    async fn send_structured(
        &self,
        room: &str,
        message: &OutboundMessage,
    ) -> anyhow::Result<String> {
        self.calls.lock().unwrap().push(ChannelCall::Structured {
            room: room.into(),
            agent: message.agent_id.clone(),
            subject: message.subject.clone(),
            body: message.body.clone(),
            hostname: message.hostname.clone(),
            event_at: message.event_at,
        });
        self.outcome()
    }

    async fn reply_structured(
        &self,
        room: &str,
        parent: &str,
        message: &OutboundMessage,
    ) -> anyhow::Result<String> {
        self.calls
            .lock()
            .unwrap()
            .push(ChannelCall::ReplyStructured {
                room: room.into(),
                parent_external: parent.into(),
                agent: message.agent_id.clone(),
                subject: message.subject.clone(),
                body: message.body.clone(),
                hostname: message.hostname.clone(),
                event_at: message.event_at,
            });
        self.outcome()
    }

    async fn fetch_since(
        &self,
        _room: &str,
        _after: Option<&str>,
    ) -> anyhow::Result<Vec<InboundMessage>> {
        Ok(Vec::new())
    }
}

struct Fixture {
    endpoint: String,
    client: Gateway,
    database: Db,
    database_path: PathBuf,
    channel: Arc<FixtureChannel>,
    actor: AgentIdentity,
    server: tokio::task::JoinHandle<()>,
    // Drop the directory after the database and server handles.
    _root: tempfile::TempDir,
}

impl Fixture {
    async fn start() -> Self {
        let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
        std::fs::create_dir_all(&fixtures).unwrap();
        let root = tempfile::tempdir_in(fixtures).unwrap();
        let checkout = root.path().join("checkout");
        std::fs::create_dir(&checkout).unwrap();
        for args in [
            vec!["init", "--quiet"],
            vec![
                "config",
                "--local",
                "remote.origin.url",
                "https://github.com/fixtures/native-client-fixture.git",
            ],
        ] {
            let result = Command::new("git")
                .arg("-C")
                .arg(&checkout)
                .args(args)
                .output()
                .unwrap();
            assert!(result.status.success(), "fixture Git setup failed");
        }
        let identity = amux::project_identity::resolve(&checkout).unwrap();
        let actor = AgentIdentity {
            session_id: Uuid::from_u128(0x8c6fa383_152b_4f0d_9d6d_cfc0730ce001),
            instance_id: Uuid::from_u128(0x8c6fa383_152b_4f0d_9d6d_cfc0730ce002),
            provider: Provider::Codex,
            os: std::env::consts::OS.into(),
            project_directory: identity.directory,
            project_id: identity.id,
            agent_id: "codex-fixture".into(),
            name: "Native Client Fixture".into(),
        };
        let database_path = root.path().join("embedded.sqlite");
        let database =
            db::open_config(&db::DatabaseConfig::sqlite(database_path.to_string_lossy())).unwrap();
        let channel = Arc::new(FixtureChannel::default());
        let mut plugins: HashMap<String, Arc<dyn ChannelPlugin>> = HashMap::new();
        plugins.insert("fixture".into(), channel.clone());
        let state = AppState {
            db: database.clone(),
            plugins: Arc::new(plugins),
            whatsapp: None,
            default_channel: "fixture".into(),
            api_key: KEY.into(),
            ui_auth_enabled: true,
            retention_days: 30,
            bot_retention_days: 7,
            artifact_operations: db::ArtifactOperationsEnvelope::default(),
            artifact_body_schema_enabled: true,
            artifact_auth_enforced: false,
            update_available: Arc::new(Mutex::new(None)),
            remote: Default::default(),
        };
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            axum::serve(listener, api_router(state)).await.unwrap();
        });
        let client = Gateway::new(&endpoint, KEY.into()).unwrap();
        Self {
            endpoint,
            client,
            database,
            database_path,
            channel,
            actor,
            server,
            _root: root,
        }
    }

    async fn project_and_requester(&self) -> SessionTarget {
        assert_eq!(
            self.client
                .resolve_project(self.actor.project_directory.clone())
                .await
                .unwrap(),
            PROJECT
        );
        let generation = Uuid::from_u128(0x8c6fa383_152b_4f0d_9d6d_cfc0730ce003);
        self.client
            .advertise_task_target(PROJECT, Some(REMOTE), &self.actor, generation)
            .await
            .unwrap();
        let requester = self
            .client
            .requester_target(PROJECT, &self.actor)
            .await
            .unwrap();
        assert_eq!(requester.generation, generation);
        assert_eq!(requester.origin.session_id, self.actor.session_id);
        assert_eq!(requester.origin.instance_id, self.actor.instance_id);
        requester
    }

    fn reopen_database(&self) -> Db {
        db::open_config(&db::DatabaseConfig::sqlite(
            self.database_path.to_string_lossy(),
        ))
        .unwrap()
    }

    async fn post(&self, project: &str, suffix: &str, body: Value) -> (reqwest::StatusCode, Value) {
        let request = reqwest::Client::new()
            .post(format!("{}/v1/projects/{project}/{suffix}", self.endpoint))
            .bearer_auth(KEY);
        let request = if suffix == "messages/human" {
            request
        } else {
            request.header("X-Agent-Id", self.actor.gateway_id())
        };
        let response = request.json(&body).send().await.unwrap();
        let status = response.status();
        (status, response.json().await.unwrap())
    }

    async fn adhoc(&self, project: &str, channel: &str) {
        let response = reqwest::Client::new()
            .post(format!("{}/v1/projects", self.endpoint))
            .bearer_auth(KEY)
            .json(&json!({"ident":project,"channel":channel}))
            .send()
            .await
            .unwrap();
        assert_eq!(response.status(), reqwest::StatusCode::OK);
        let value: Value = response.json().await.unwrap();
        assert_eq!(value["kind"], "adhoc");
        assert_eq!(value["room_id"], "");
    }

    fn stored_message(&self, project: &str, id: i64) -> db::Message {
        db::get_message_by_id(&self.database.lock().unwrap(), project, id)
            .unwrap()
            .unwrap()
    }

    fn message_count(&self, project: &str) -> i64 {
        self.database
            .lock()
            .unwrap()
            .query_row(
                "SELECT COUNT(*) FROM messages WHERE project_ident=?1",
                [project],
                |row| row.get(0),
            )
            .unwrap()
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.server.abort();
    }
}

fn origin(actor: &AgentIdentity) -> Value {
    json!({
        "session_id":actor.session_id,
        "instance_id":actor.instance_id,
        "provider":actor.provider.command(),
        "os":actor.os,
    })
}

fn assert_stored_fields(
    fixture: &Fixture,
    project: &str,
    response: &Value,
    expected: Value,
) -> db::Message {
    let message = fixture.stored_message(project, response["message_id"].as_i64().unwrap());
    let actual = serde_json::to_value(&message).unwrap();
    for (key, value) in expected.as_object().unwrap() {
        assert_eq!(&actual[key], value, "stored message field {key}");
    }
    message
}

#[tokio::test]
async fn lazy_room_creation_and_structured_message_aliases_preserve_plugin_and_storage_contracts() {
    let fixture = Fixture::start().await;
    let project = "lazy-channel-fixture";
    fixture.adhoc(project, "fixture").await;
    assert!(fixture.channel.calls.lock().unwrap().is_empty());
    let room = format!("fixture-room-{project}");
    for _ in 0..2 {
        let (status, response) = fixture.post(project, "room", json!({})).await;
        assert_eq!(status, reqwest::StatusCode::OK);
        assert_eq!(response["room_id"], room);
    }
    // Upstream's lazy helper calls register_room after ensure_room, whose trait
    // contract already registers the room. Preserve these two idempotent calls;
    // there is exactly one ensure and one durable room, never a second creation.
    assert_eq!(
        *fixture.channel.calls.lock().unwrap(),
        [
            ChannelCall::EnsureRoom(project.into()),
            ChannelCall::RegisterRoom(room.clone(), None),
            ChannelCall::RegisterRoom(room.clone(), None),
        ]
    );
    let event_at = 1_710_000_000_123_i64;
    let (status, response) = fixture.post(project, "messages", json!({
        "body":"Primary body\nline two", "content":"ignored alias",
        "subject":"  Explicit subject  ", "hostname":"  fixture-host  ", "event_at":event_at,
    })).await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::Structured {
            room: room.clone(),
            agent: fixture.actor.gateway_id(),
            subject: "Explicit subject".into(),
            body: "Primary body\nline two".into(),
            hostname: "fixture-host".into(),
            event_at,
        }
    );
    assert_stored_fields(
        &fixture,
        project,
        &response,
        json!({
            "content":"Primary body\nline two", "subject":"Explicit subject", "hostname":"fixture-host",
            "event_at":event_at, "source":"agent", "author_kind":"agent", "message_type":"message",
            "external_message_id":"fixture-external-message-1", "parent_message_id":null,
        }),
    );
    let (status, alias) = fixture
        .post(
            project,
            "messages",
            json!({
                "content":"\n  Alias subject  \nMore text", "subject":"   ", "hostname":"  ",
            }),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::OK);
    let alias_message = assert_stored_fields(
        &fixture,
        project,
        &alias,
        json!({
            "content":"\n  Alias subject  \nMore text", "subject":"Alias subject", "hostname":fixture.actor.gateway_id(),
            "external_message_id":"fixture-external-message-2",
        }),
    );
    assert!(alias_message.event_at.unwrap() > 0);
    let calls_before = fixture.channel.calls.lock().unwrap().len();
    let (status, _) = fixture
        .post(
            project,
            "messages",
            json!({"body":"", "content":"must not win"}),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::BAD_REQUEST);
    assert_eq!(fixture.channel.calls.lock().unwrap().len(), calls_before);
    assert_eq!(fixture.message_count(project), 2);
    fixture
        .channel
        .fail_next("injected structured delivery failure");
    let (status, error) = fixture
        .post(
            project,
            "messages",
            json!({"body":"not persisted after failed agent send"}),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::INTERNAL_SERVER_ERROR);
    assert_eq!(error["error"], "injected structured delivery failure");
    assert_eq!(fixture.message_count(project), 2);
    let calls = fixture.channel.calls.lock().unwrap();
    assert_eq!(
        calls
            .iter()
            .filter(|call| matches!(call, ChannelCall::EnsureRoom(_)))
            .count(),
        1
    );
    assert_eq!(
        calls
            .iter()
            .filter(|call| matches!(call, ChannelCall::RegisterRoom(_, _)))
            .count(),
        2
    );
    let stored_room = db::get_project(&fixture.database.lock().unwrap(), project)
        .unwrap()
        .unwrap();
    assert_eq!(stored_room.room_id, room);
}

#[tokio::test]
async fn plugin_reply_and_action_routes_select_external_parent_or_structured_fallback() {
    let fixture = Fixture::start().await;
    fixture.project_and_requester().await;
    let room = format!("fixture-room-{PROJECT}");
    let root = fixture
        .client
        .send_message(PROJECT, "External root".into(), None, &fixture.actor)
        .await
        .unwrap();
    let root_id = root["message_id"].as_i64().unwrap();
    let external = root["external_message_id"].as_str().unwrap();
    let event_at = 1_710_000_000_456_i64;
    let (status, reply) = fixture.post(PROJECT, &format!("messages/{root_id}/reply"), json!({
        "body":"Primary reply", "content":"ignored reply alias", "subject":" Reply subject ",
        "hostname":" reply-host ", "event_at":event_at,
    })).await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(reply["parent_message_id"], root_id);
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::ReplyStructured {
            room: room.clone(),
            parent_external: external.into(),
            agent: fixture.actor.gateway_id(),
            subject: "Reply subject".into(),
            body: "Primary reply".into(),
            hostname: "reply-host".into(),
            event_at,
        }
    );
    assert_stored_fields(
        &fixture,
        PROJECT,
        &reply,
        json!({
            "parent_message_id":root_id, "message_type":"reply", "content":"Primary reply",
            "subject":"Reply subject", "hostname":"reply-host", "event_at":event_at,
        }),
    );
    let local_parent = {
        let conn = fixture.database.lock().unwrap();
        db::insert_message(
            &conn,
            &db::Message {
                id: 0,
                project_ident: PROJECT.into(),
                source: "user".into(),
                external_message_id: None,
                content: "Imported local-only parent".into(),
                sent_at: db::now_ms(),
                confirmed_at: None,
                parent_message_id: None,
                agent_id: None,
                message_type: "message".into(),
                subject: None,
                hostname: None,
                event_at: None,
                deliver_to_agents: true,
                author_kind: Some("human".into()),
                resolved_at: None,
                resolved_by: None,
            },
        )
        .unwrap()
    };
    let (status, fallback) = fixture
        .post(
            PROJECT,
            &format!("messages/{local_parent}/reply"),
            json!({
                "content":"Alias reply", "event_at":event_at+1,
            }),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::Structured {
            room: room.clone(),
            agent: fixture.actor.gateway_id(),
            subject: "Alias reply".into(),
            body: "Alias reply".into(),
            hostname: fixture.actor.gateway_id(),
            event_at: event_at + 1,
        }
    );
    assert_stored_fields(
        &fixture,
        PROJECT,
        &fallback,
        json!({
            "parent_message_id":local_parent, "message_type":"reply", "content":"Alias reply",
        }),
    );
    let (status, action) = fixture.post(PROJECT, &format!("messages/{root_id}/action"), json!({
        "body":"\n  Investigate failure  \nDetails", "message":"ignored action alias", "event_at":event_at+2,
    })).await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::ReplyStructured {
            room: room.clone(),
            parent_external: external.into(),
            agent: fixture.actor.gateway_id(),
            subject: "[ACTION] Investigate failure".into(),
            body: "\n  Investigate failure  \nDetails".into(),
            hostname: fixture.actor.gateway_id(),
            event_at: event_at + 2,
        }
    );
    assert_stored_fields(
        &fixture,
        PROJECT,
        &action,
        json!({
            "parent_message_id":root_id, "message_type":"action", "content":"\n  Investigate failure  \nDetails",
            "subject":"[ACTION] Investigate failure", "event_at":event_at+2,
        }),
    );
    let (status, alias_action) = fixture
        .post(
            PROJECT,
            &format!("messages/{local_parent}/action"),
            json!({
                "message":"Alias action", "subject":"Operator label", "event_at":event_at+3,
            }),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::Structured {
            room: room.clone(),
            agent: fixture.actor.gateway_id(),
            subject: "Operator label".into(),
            body: "Alias action".into(),
            hostname: fixture.actor.gateway_id(),
            event_at: event_at + 3,
        }
    );
    assert_stored_fields(
        &fixture,
        PROJECT,
        &alias_action,
        json!({
            "parent_message_id":local_parent, "message_type":"action", "content":"Alias action",
            "subject":"Operator label", "event_at":event_at+3,
        }),
    );
    let count = fixture.message_count(PROJECT);
    let calls = fixture.channel.calls.lock().unwrap().len();
    for (suffix, body) in [
        (
            format!("messages/{root_id}/reply"),
            json!({"body":"", "content":"must not win"}),
        ),
        (
            format!("messages/{root_id}/action"),
            json!({"body":"", "message":"must not win"}),
        ),
    ] {
        let (status, _) = fixture.post(PROJECT, &suffix, body).await;
        assert_eq!(status, reqwest::StatusCode::BAD_REQUEST);
    }
    let (status, _) = fixture
        .post(
            PROJECT,
            "messages/999999/reply",
            json!({"body":"missing parent"}),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::NOT_FOUND);
    assert_eq!(fixture.message_count(PROJECT), count);
    assert_eq!(fixture.channel.calls.lock().unwrap().len(), calls);
}

#[tokio::test]
async fn human_messages_persist_before_raw_delivery_and_report_optional_channel_failures() {
    let fixture = Fixture::start().await;
    fixture.project_and_requester().await;
    let room = format!("fixture-room-{PROJECT}");
    let (status, root) = fixture
        .post(
            PROJECT,
            "messages/human",
            json!({"content":"  Human root\n ", "author":" Ada "}),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(root["delivered"], true);
    assert_eq!(root["delivery_error"], Value::Null);
    let root_id = root["message_id"].as_i64().unwrap();
    let root_message = assert_stored_fields(
        &fixture,
        PROJECT,
        &root,
        json!({
            "source":"user", "content":"Human root", "author_kind":"human", "deliver_to_agents":true,
            "parent_message_id":null, "external_message_id":"fixture-external-message-1", "message_type":"message",
        }),
    );
    assert!(root_message.confirmed_at.is_none());
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::Raw {
            room: room.clone(),
            content: "**Ada** (via gateway): Human root".into(),
        }
    );
    let (status, reply) = fixture
        .post(
            PROJECT,
            "messages/human",
            json!({
                "content":"Human reply", "parent_message_id":root_id.to_string(),
            }),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(reply["delivered"], true);
    let reply_id = reply["message_id"].as_i64().unwrap();
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::RawReply {
            room: room.clone(),
            parent_external: "fixture-external-message-1".into(),
            content: "**user** (via gateway): Human reply".into(),
        }
    );
    assert_stored_fields(
        &fixture,
        PROJECT,
        &reply,
        json!({"parent_message_id":root_id,"message_type":"reply","external_message_id":"fixture-external-message-2"}),
    );
    let (status, nested) = fixture.post(PROJECT, "messages/human", json!({
        "content":"Nested reply", "parent_message_id":reply_id, "author":"Grace", "resolve":"on",
    })).await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(nested["parent_message_id"], root_id);
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::RawReply {
            room: room.clone(),
            parent_external: "fixture-external-message-1".into(),
            content: "**Grace** (via gateway): Nested reply".into(),
        }
    );
    let resolved = fixture.stored_message(PROJECT, root_id);
    assert!(resolved.resolved_at.is_some());
    assert_eq!(resolved.resolved_by.as_deref(), Some("user"));
    fixture.channel.fail_next("injected raw delivery failure");
    let (status, unsent) = fixture
        .post(
            PROJECT,
            "messages/human",
            json!({"content":"Saved before external error"}),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(unsent["delivered"], false);
    assert_eq!(unsent["delivery_error"], "injected raw delivery failure");
    assert_stored_fields(
        &fixture,
        PROJECT,
        &unsent,
        json!({
            "source":"user", "content":"Saved before external error", "external_message_id":null, "deliver_to_agents":true,
        }),
    );
    let missing = "missing-channel-fixture";
    fixture.adhoc(missing, "not-configured").await;
    let calls = fixture.channel.calls.lock().unwrap().len();
    let (status, optional) = fixture
        .post(
            missing,
            "messages/human",
            json!({"content":"Durable without an external channel"}),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::OK);
    assert_eq!(optional["delivered"], false);
    assert_eq!(
        optional["delivery_error"],
        "channel plugin 'not-configured' is not configured"
    );
    assert_stored_fields(
        &fixture,
        missing,
        &optional,
        json!({"external_message_id":null,"author_kind":"human","deliver_to_agents":true}),
    );
    assert_eq!(fixture.channel.calls.lock().unwrap().len(), calls);
    let count = fixture.message_count(PROJECT);
    let (status, _) = fixture
        .post(PROJECT, "messages/human", json!({"content":"   "}))
        .await;
    assert_eq!(status, reqwest::StatusCode::BAD_REQUEST);
    let (status, _) = fixture
        .post(
            PROJECT,
            "messages/human",
            json!({"content":"No parent exists", "parent_message_id":999999}),
        )
        .await;
    assert_eq!(status, reqwest::StatusCode::NOT_FOUND);
    assert_eq!(fixture.message_count(PROJECT), count);
    assert_eq!(fixture.channel.calls.lock().unwrap().len(), calls);
}

#[tokio::test]
async fn native_rest_clients_register_send_claim_watch_comment_and_cancel_on_real_backend() {
    let fixture = Fixture::start().await;
    let requester = fixture.project_and_requester().await;
    let room = format!("fixture-room-{PROJECT}");
    assert_eq!(
        *fixture.channel.calls.lock().unwrap(),
        [
            ChannelCall::EnsureRoom(PROJECT.into()),
            ChannelCall::RegisterRoom(room.clone(), None),
        ]
    );
    let published = fixture
        .client
        .send_message(
            PROJECT,
            "Durable native handoff\nwith actual JSON escaping: \"ready\".".into(),
            Some("Native message".into()),
            &fixture.actor,
        )
        .await
        .unwrap();
    let message_id = published["message_id"].as_i64().unwrap();
    assert_eq!(
        published["external_message_id"],
        "fixture-external-message-1"
    );
    let event_at = fixture
        .stored_message(PROJECT, message_id)
        .event_at
        .unwrap();
    assert!(event_at > 0);
    assert_eq!(
        fixture.channel.calls.lock().unwrap().last().unwrap(),
        &ChannelCall::Structured {
            room: room.clone(),
            agent: fixture.actor.gateway_id(),
            subject: "Native message".into(),
            body: "Durable native handoff\nwith actual JSON escaping: \"ready\".".into(),
            hostname: fixture.actor.gateway_id(),
            event_at,
        }
    );
    let task = fixture
        .client
        .create_task_with_requester(
            PROJECT,
            json!({"title":"Native task", "specification":"Persist canonical state"}),
            &fixture.actor,
            &requester,
        )
        .await
        .unwrap();
    let id: Uuid = serde_json::from_value(task["id"].clone()).unwrap();
    assert_eq!(task["requester"], json!(requester));
    assert_eq!(task["owner_origin"], origin(&fixture.actor));
    fixture
        .client
        .claim(PROJECT, &id.to_string(), &fixture.actor)
        .await
        .unwrap();
    let mut peer = fixture.actor.clone();
    peer.session_id = Uuid::from_u128(0x8c6fa383_152b_4f0d_9d6d_cfc0730ce004);
    peer.name = "Different Fixture".into();
    assert!(
        fixture
            .client
            .claim(PROJECT, &id.to_string(), &peer)
            .await
            .is_err()
    );
    assert!(
        fixture
            .client
            .set_task_requester(PROJECT, id, None, Some(&peer))
            .await
            .is_err()
    );
    fixture
        .client
        .set_task_requester(PROJECT, id, None, Some(&fixture.actor))
        .await
        .unwrap();
    fixture
        .client
        .set_task_requester(PROJECT, id, Some(&requester), Some(&fixture.actor))
        .await
        .unwrap();
    let comment = fixture
        .client
        .request(
            Method::POST,
            PROJECT,
            &[&id.to_string(), "comments"],
            Some(json!({"content":"Verified through the native client"})),
            Some(&fixture.actor),
        )
        .await
        .unwrap();
    assert_eq!(comment["origin"], origin(&fixture.actor));
    let cancelled = fixture
        .client
        .cancel_task(PROJECT, id, "fixture complete", &fixture.actor)
        .await
        .unwrap();
    assert_eq!(cancelled["task"]["status"], "cancelled");
    assert_eq!(
        fixture.client.cancellation(PROJECT, id).await.unwrap(),
        cancelled["cancellation"]
    );

    // Reopen the on-disk database instead of trusting HTTP acknowledgments alone.
    let reopened = fixture.reopen_database();
    let conn = reopened.lock().unwrap();
    let project = db::get_project(&conn, PROJECT).unwrap().unwrap();
    assert_eq!(project.canonical_remote.as_deref(), Some(REMOTE));
    assert_eq!(project.room_id, room);
    let message = db::get_message_by_id(&conn, PROJECT, message_id)
        .unwrap()
        .unwrap();
    assert_eq!(
        message.external_message_id.as_deref(),
        Some("fixture-external-message-1")
    );
    assert_eq!(
        message.agent_id.as_deref(),
        Some(fixture.actor.gateway_id().as_str())
    );
    assert_eq!(
        message.content,
        "Durable native handoff\nwith actual JSON escaping: \"ready\"."
    );
    let stored = db::get_task_detail(&conn, PROJECT, &id.to_string())
        .unwrap()
        .unwrap();
    assert_eq!(stored.task.status, "cancelled");
    assert_eq!(
        serde_json::to_value(stored.task.owner_origin).unwrap(),
        origin(&fixture.actor)
    );
    assert_eq!(
        serde_json::to_value(stored.task.requester).unwrap(),
        json!(requester)
    );
    assert!(
        stored
            .comments
            .iter()
            .any(|c| c.content == "Verified through the native client")
    );
    let cancellation_reason: String = conn
        .query_row(
            "SELECT reason FROM task_cancellations WHERE task_id=?1",
            [id.to_string()],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(cancellation_reason, "fixture complete");
}

async fn next_frame(socket: &mut Socket) -> Value {
    tokio::time::timeout(Duration::from_secs(10), gateway_stream::next_json(socket))
        .await
        .expect("embedded stream produced no frame")
        .unwrap()
}

async fn acknowledge(socket: &mut Socket, event: &Value, status: &str) -> i64 {
    let id = event["event"]["id"].as_i64().unwrap();
    gateway_stream::send_json(
        socket,
        json!({"type":"ack", "event_id":id, "status":status}),
    )
    .await
    .unwrap();
    assert_eq!(
        next_frame(socket).await,
        json!({"type":"recorded", "event_id":id, "status":status})
    );
    id
}

#[tokio::test]
async fn native_websocket_subscribe_receipts_and_reconnect_use_durable_real_events() {
    let fixture = Fixture::start().await;
    let requester = fixture.project_and_requester().await;
    assert!(
        gateway_stream::connect(&fixture.endpoint, "wrong-fixture-key", Uuid::new_v4(), None)
            .await
            .is_err()
    );
    let consumer = Uuid::from_u128(0x8c6fa383_152b_4f0d_9d6d_cfc0730ce005);
    let mut socket = gateway_stream::connect(&fixture.endpoint, KEY, consumer, None)
        .await
        .unwrap();
    let subscribed = next_frame(&mut socket).await;
    assert_eq!(subscribed["type"], "subscribed");
    assert_eq!(subscribed["protocol_version"], 1);
    assert_eq!(subscribed["consumer_id"], consumer.to_string());
    assert_eq!(subscribed["cursor"], 0);
    assert_eq!(subscribed["latest_event_id"], 0);

    let task = fixture
        .client
        .create_task_with_requester(
            PROJECT,
            json!({"title":"Streaming native task"}),
            &fixture.actor,
            &requester,
        )
        .await
        .unwrap();
    let id: Uuid = serde_json::from_value(task["id"].clone()).unwrap();
    let created = next_frame(&mut socket).await;
    assert_eq!(created["type"], "event");
    assert_eq!(created["event"]["kind"], "task_created");
    assert_eq!(created["event"]["task"]["id"], id.to_string());
    assert_eq!(created["event"]["project_ident"], PROJECT);
    assert_eq!(created["event"]["canonical_remote"], REMOTE);
    assert_eq!(created["event"]["origin"], origin(&fixture.actor));
    assert_eq!(created["event"]["task"]["requester"], json!(requester));
    let created_id = acknowledge(&mut socket, &created, "received").await;
    {
        let conn = fixture.database.lock().unwrap();
        let stored = db::get_task_detail(&conn, PROJECT, &id.to_string())
            .unwrap()
            .unwrap();
        assert_eq!(
            stored.task.status, "todo",
            "delivery receipt cannot claim work"
        );
        assert!(stored.task.started_at.is_none());
    }
    fixture
        .client
        .request(
            Method::POST,
            PROJECT,
            &[&id.to_string(), "comments"],
            Some(json!({"content":"Native stream comment"})),
            Some(&fixture.actor),
        )
        .await
        .unwrap();
    let commented = next_frame(&mut socket).await;
    assert_eq!(commented["event"]["kind"], "task_commented");
    assert_eq!(
        commented["event"]["comment"]["content"],
        "Native stream comment"
    );
    let comment_id = acknowledge(&mut socket, &commented, "queued").await;
    assert!(comment_id > created_id);
    gateway_stream::send_json(&mut socket, json!({"type":"heartbeat"}))
        .await
        .unwrap();
    assert_eq!(
        next_frame(&mut socket).await,
        json!({"type":"heartbeat_ack"})
    );
    socket.close(None).await.unwrap();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    loop {
        let connected: bool = fixture
            .database
            .lock()
            .unwrap()
            .query_row(
                "SELECT connected FROM task_stream_consumers WHERE consumer_id=?1",
                [consumer.to_string()],
                |row| row.get(0),
            )
            .unwrap();
        if !connected {
            break;
        }
        assert!(
            tokio::time::Instant::now() < deadline,
            "stream did not release its consumer"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let mut resumed = gateway_stream::connect(&fixture.endpoint, KEY, consumer, Some(comment_id))
        .await
        .unwrap();
    let subscribed = next_frame(&mut resumed).await;
    assert_eq!(subscribed["cursor"], comment_id);
    assert_eq!(subscribed["latest_event_id"], comment_id);
    fixture
        .client
        .claim(PROJECT, &id.to_string(), &fixture.actor)
        .await
        .unwrap();
    fixture
        .client
        .request(
            Method::PATCH,
            PROJECT,
            &[&id.to_string()],
            Some(json!({"status":"done"})),
            Some(&fixture.actor),
        )
        .await
        .unwrap();
    let completed = next_frame(&mut resumed).await;
    assert_eq!(completed["event"]["kind"], "task_completed");
    assert_eq!(completed["event"]["task"]["status"], "done");
    assert_eq!(completed["event"]["task"]["requester"], json!(requester));
    let completed_id = acknowledge(&mut resumed, &completed, "injected").await;
    assert!(
        completed_id > comment_id,
        "reconnect must not replay acknowledged historical events"
    );
    resumed.close(None).await.unwrap();
    let reopened = fixture.reopen_database();
    let conn = reopened.lock().unwrap();
    let (cursor, count): (i64, i64) = conn.query_row(
        "SELECT cursor,(SELECT COUNT(*) FROM task_event_deliveries WHERE consumer_id=?1) FROM task_stream_consumers WHERE consumer_id=?1", [consumer.to_string()], |row| Ok((row.get(0)?,row.get(1)?))
    ).unwrap();
    assert_eq!(cursor, completed_id);
    assert_eq!(count, 3);
    let status: String = conn
        .query_row(
            "SELECT status FROM task_event_deliveries WHERE consumer_id=?1 AND event_id=?2",
            [consumer.to_string(), completed_id.to_string()],
            |row| row.get(0),
        )
        .unwrap();
    assert_eq!(status, "injected");
    assert_eq!(
        db::get_task_detail(&conn, PROJECT, &id.to_string())
            .unwrap()
            .unwrap()
            .task
            .status,
        "done"
    );
}
