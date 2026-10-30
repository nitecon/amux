//! Compatibility startup from agent-gateway 986d84544aae086579c72349a1afc366cef6b589.
//! The caller owns the listener and Tokio runtime; no browser UI or updater runs here.
use crate::{
    api_router,
    channel::{ChannelPlugin, PluginEvent},
    db, task_events, AppState,
};
use anyhow::{Context, Result};
use std::{collections::HashMap, net::SocketAddr, sync::Arc, time::Duration};
use tokio::{net::TcpListener, sync::mpsc};

/// Existing gateway environment configuration. Credentials deliberately have no Debug output.
pub struct RuntimeConfig {
    pub listen_addr: SocketAddr,
    pub database: db::DatabaseConfig,
    pub api_key: String,
    pub default_channel: String,
    pub retention_days: u64,
    pub bot_retention_days: u64,
    pub artifact_operations: db::ArtifactOperationsEnvelope,
    pub artifact_body_schema_enabled: bool,
    pub artifact_auth_enforced: bool,
    pub ui_auth_enabled: bool,
    #[cfg(feature = "discord")]
    pub discord: Option<crate::channels::discord::DiscordConfig>,
    #[cfg(feature = "whatsapp")]
    pub whatsapp: Option<crate::channels::whatsapp::WhatsAppConfig>,
}

impl RuntimeConfig {
    /// Read configuration only. The caller may load its .env file before calling this.
    /// No database, external plugin, network listener or process is started here.
    pub fn from_env() -> Result<Self> {
        Self::read(None)
    }
    /// Native hosting has no shared bearer bootstrap credential.
    pub fn from_env_secure() -> Result<Self> {
        Self::read(Some(uuid::Uuid::now_v7().to_string()))
    }
    fn read(fallback: Option<String>) -> Result<Self> {
        let api_key = std::env::var("GATEWAY_API_KEY")
            .or_else(|error| fallback.ok_or(error))
            .map_err(|_| {
                anyhow::anyhow!("Missing required environment variable: GATEWAY_API_KEY")
            })?;
        let default_channel =
            std::env::var("DEFAULT_CHANNEL").unwrap_or_else(|_| crate::messaging::INTERNAL.into());
        let host = std::env::var("GATEWAY_HOST").unwrap_or_else(|_| "0.0.0.0".into());
        let port: u16 = std::env::var("GATEWAY_PORT")
            .unwrap_or_else(|_| "7913".into())
            .parse()
            .context("GATEWAY_PORT must be a u16")?;
        let listen_addr = format!("{host}:{port}")
            .parse()
            .context("parse listen address")?;
        let database = db::DatabaseConfig::from_env().context("load database config")?;
        let retention_days = std::env::var("MESSAGE_RETENTION_DAYS")
            .unwrap_or_else(|_| "30".into())
            .parse()
            .context("MESSAGE_RETENTION_DAYS must be a u64")?;
        let bot_retention_days = std::env::var("BOT_MESSAGE_RETENTION_DAYS")
            .unwrap_or_else(|_| "7".into())
            .parse()
            .context("BOT_MESSAGE_RETENTION_DAYS must be a u64")?;
        let artifact_operations = db::ArtifactOperationsEnvelope::from_env()
            .context("load artifact operations envelope")?;
        let artifact_body_schema_enabled = env_flag("GATEWAY_ARTIFACT_BODY_SCHEMA_ENABLED", true)?;
        let artifact_auth_enforced = env_flag("GATEWAY_ARTIFACT_AUTH_ENFORCED", false)?;
        let ui_auth_enabled = env_flag("GATEWAY_UI_AUTH", true)?;

        #[cfg(feature = "discord")]
        let discord = match (
            std::env::var("DISCORD_BOT_TOKEN")
                .ok()
                .filter(|value| !value.is_empty()),
            std::env::var("DISCORD_GUILD_ID")
                .ok()
                .filter(|value| !value.is_empty()),
        ) {
            (Some(token), Some(guild_id)) => Some(crate::channels::discord::DiscordConfig {
                token,
                guild_id: guild_id.parse().context("DISCORD_GUILD_ID must be a u64")?,
                category_id: std::env::var("DISCORD_CATEGORY_ID")
                    .ok()
                    .filter(|value| !value.is_empty())
                    .map(|value| value.parse().context("DISCORD_CATEGORY_ID must be a u64"))
                    .transpose()?,
            }),
            _ => None,
        };
        #[cfg(feature = "whatsapp")]
        let whatsapp = crate::channels::whatsapp::WhatsAppConfig::from_env()
            .context("load WhatsApp channel config")?;

        Ok(Self {
            listen_addr,
            database,
            api_key,
            default_channel,
            retention_days,
            bot_retention_days,
            artifact_operations,
            artifact_body_schema_enabled,
            artifact_auth_enforced,
            ui_auth_enabled,
            #[cfg(feature = "discord")]
            discord,
            #[cfg(feature = "whatsapp")]
            whatsapp,
        })
    }
}

/// Run the real compatibility backend on the caller-supplied listener.
///
/// External plugins retain their original startup behavior. In particular the
/// imported Discord plugin owns a detached worker and exposes no shutdown hook;
/// an external-channel runtime therefore follows the hosting process lifetime.
pub async fn serve(config: RuntimeConfig, listener: TcpListener) -> Result<()> {
    let prepared = prepare(config).await?;
    tracing::info!(
        "Amux compatibility gateway listening on http://{}",
        listener.local_addr()?
    );
    axum::serve(listener, prepared.app.clone()).await?;
    Ok(())
}

/// Initialized backend and owned workers, shared by legacy and secure listeners.
pub struct Prepared {
    pub app: axum::Router,
    pub state: AppState,
    _workers: Workers,
}
pub async fn prepare(config: RuntimeConfig) -> Result<Prepared> {
    if let Some(path) = config.database.sqlite_path() {
        if path != ":memory:" {
            if let Some(parent) = std::path::Path::new(path)
                .parent()
                .filter(|parent| !parent.as_os_str().is_empty())
            {
                std::fs::create_dir_all(parent).context("create db directory")?;
            }
        }
    }
    let database = db::open_config(&config.database)?;
    tracing::info!(
        "Database opened with backend={} location={}",
        database.backend(),
        config.database.display_location()
    );

    #[allow(unused_mut)]
    let mut plugins: HashMap<String, Arc<dyn ChannelPlugin>> = HashMap::new();
    #[cfg(feature = "discord")]
    if let Some(discord) = config.discord {
        plugins.insert(
            "discord".into(),
            Arc::new(crate::channels::discord::DiscordPlugin::new(discord)),
        );
        tracing::info!("Registered channel plugin: discord");
    } else {
        tracing::warn!(
            "Discord plugin disabled: DISCORD_BOT_TOKEN and DISCORD_GUILD_ID are not both set"
        );
    }
    #[cfg(feature = "whatsapp")]
    let whatsapp = config.whatsapp.map(|whatsapp| {
        let plugin = Arc::new(crate::channels::whatsapp::WhatsAppPlugin::new(whatsapp));
        plugins.insert("whatsapp".into(), plugin.clone());
        tracing::info!("Registered channel plugin: whatsapp");
        plugin
    });
    #[cfg(feature = "whatsapp")]
    if whatsapp.is_none() {
        tracing::warn!("WhatsApp plugin disabled: WHATSAPP_ACCESS_TOKEN, WHATSAPP_PHONE_NUMBER_ID, and WHATSAPP_WEBHOOK_VERIFY_TOKEN are not set");
    }
    if config.default_channel != crate::messaging::INTERNAL
        && !plugins.contains_key(&config.default_channel)
    {
        tracing::warn!(
            "DEFAULT_CHANNEL='{}' is not among the enabled plugins: {:?}",
            config.default_channel,
            plugins.keys().collect::<Vec<_>>()
        );
    }
    let existing_projects = {
        let conn = database.lock().unwrap();
        db::all_projects(&conn)?
    };
    for project in existing_projects
        .iter()
        .filter(|project| !project.room_id.is_empty())
    {
        if let Some(plugin) = plugins.get(&project.channel_name) {
            plugin.register_room(&project.room_id, project.last_msg_id.as_deref());
        }
    }
    tracing::info!(
        "Registered {} existing project room(s)",
        existing_projects.len()
    );

    let (tx, rx) = mpsc::channel::<PluginEvent>(256);
    let mut workers = Workers(Vec::new());
    let remote = crate::plugins::remote::Remote::default();
    let mut controllers = crate::plugins::plugins(&remote);
    controllers.push(Box::new(crate::plugins::retention::Retention::new(
        config.retention_days,
        config.bot_retention_days,
    )));
    let (bus, router) =
        crate::router::start(database.clone(), crate::plugins::events(), controllers)?;
    remote.attach(bus.clone());
    workers.0.push(router);
    // Drain input before starting transports: synchronous backfill may exceed
    // the bounded queue capacity during ChannelPlugin::start.
    workers.0.push(spawn_inbound_source(bus.clone(), rx));
    for plugin in plugins.values() {
        plugin.start(tx.clone()).await?;
        tracing::info!("Started channel plugin: {}", plugin.name());
    }
    drop(tx);
    bus.emit(
        "runtime",
        crate::plugins::retention::STARTUP,
        1,
        serde_json::json!({}),
        None,
    )
    .await?;
    workers.0.push(crate::plugins::clock::spawn(bus.clone()));
    workers.0.push(crate::plugins::tasks::spawn(bus));

    let state = AppState {
        db: database,
        plugins: Arc::new(plugins),
        #[cfg(feature = "whatsapp")]
        whatsapp,
        default_channel: config.default_channel,
        api_key: config.api_key,
        ui_auth_enabled: config.ui_auth_enabled,
        retention_days: config.retention_days,
        bot_retention_days: config.bot_retention_days,
        artifact_operations: config.artifact_operations,
        artifact_body_schema_enabled: config.artifact_body_schema_enabled,
        artifact_auth_enforced: config.artifact_auth_enforced,
        remote,
    };
    task_events::recover(&state.db.lock().unwrap())?;
    let app = api_router(state.clone());
    // This remains outside API bearer authentication, exactly as upstream.
    // When compiled but unconfigured, the handler returns its existing 503.
    #[cfg(feature = "whatsapp")]
    let app = axum::Router::new()
        .route(
            "/webhooks/whatsapp",
            axum::routing::get(crate::channels::whatsapp::verify_webhook)
                .post(crate::channels::whatsapp::receive_webhook),
        )
        .with_state(state.clone())
        // Upstream merges the protected API last, including its fallback.
        .merge(app);
    Ok(Prepared {
        app,
        state,
        _workers: workers,
    })
}

struct Workers(Vec<tokio::task::JoinHandle<()>>);
impl Drop for Workers {
    fn drop(&mut self) {
        for worker in &self.0 {
            worker.abort();
        }
    }
}

fn spawn_inbound_source(
    bus: crate::router::Bus,
    mut rx: mpsc::Receiver<PluginEvent>,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        while let Some(event) = rx.recv().await {
            let PluginEvent::Message {
                channel_name,
                room_id,
                message,
            } = event;
            let source = format!("channel:{channel_name}");
            let payload = serde_json::json!({
                "channel_name": channel_name,
                "room_id": room_id,
                "message": {"id": message.id, "content": message.content, "author_kind": message.author_kind},
            });
            // Keep this input until durable acceptance. The bounded channel
            // applies backpressure; its transport acceptance remains in-memory.
            loop {
                match bus
                    .emit(
                        &source,
                        crate::plugins::inbound::RECEIVED,
                        1,
                        payload.clone(),
                        None,
                    )
                    .await
                {
                    Ok(_) => break,
                    Err(error) => {
                        tracing::warn!("Inbound event persistence failed; retrying: {error}");
                        tokio::time::sleep(Duration::from_secs(1)).await;
                    }
                }
            }
        }
    })
}

fn env_flag(key: &str, default: bool) -> Result<bool> {
    match std::env::var(key) {
        Ok(value) => parse_flag(key, &value),
        Err(std::env::VarError::NotPresent) => Ok(default),
        Err(error) => Err(error).with_context(|| format!("read {key}")),
    }
}

fn parse_flag(key: &str, value: &str) -> Result<bool> {
    match value.trim().to_ascii_lowercase().as_str() {
        "1" | "true" | "yes" | "on" => Ok(true),
        "0" | "false" | "no" | "off" => Ok(false),
        _ => anyhow::bail!("{key} must be a boolean flag"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn inbound_source_drains_startup_backlog_and_retries_persistence_with_backpressure() {
        let database = db::open(":memory:").unwrap();
        {
            let conn = database.lock().unwrap();
            conn.execute_batch(
                "INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','room',0);
                 CREATE TRIGGER fail_ingress BEFORE INSERT ON router_events
                 BEGIN SELECT RAISE(ABORT,'injected bus persistence failure'); END;",
            ).unwrap();
        }
        let (bus, router) = crate::router::start(
            database.clone(),
            vec![crate::plugins::inbound::definition()],
            vec![Box::new(crate::plugins::inbound::Inbound)],
        )
        .unwrap();
        let (tx, rx) = mpsc::channel(256);
        let source = spawn_inbound_source(bus, rx);
        // Like a ChannelPlugin::start with synchronous backfill larger than the
        // queue: the already running source must drain input during startup.
        let producer = tokio::spawn(async move {
            for id in 0..300 {
                tx.send(PluginEvent::Message {
                    channel_name: "discord".into(),
                    room_id: "room".into(),
                    message: crate::channel::InboundMessage {
                        id: id.to_string(),
                        content: "hello".into(),
                        sender: "human".into(),
                        author_kind: "human".into(),
                    },
                })
                .await
                .unwrap();
            }
        });
        tokio::time::sleep(Duration::from_millis(20)).await;
        assert!(
            !producer.is_finished(),
            "failed persistence holds input and fills the bounded queue"
        );
        database
            .lock()
            .unwrap()
            .execute_batch("DROP TRIGGER fail_ingress")
            .unwrap();
        tokio::time::timeout(Duration::from_secs(10), async {
            producer.await.unwrap();
            source.await.unwrap();
        })
        .await
        .unwrap();
        let count: i64 = database
            .lock()
            .unwrap()
            .query_row(
                "SELECT COUNT(*) FROM router_events WHERE id=?1",
                [crate::plugins::inbound::RECEIVED],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(
            count, 300,
            "all startup input reached durable acceptance exactly once"
        );
        router.abort();
    }

    #[test]
    fn existing_boolean_environment_spellings_are_preserved() {
        for value in ["1", "true", "YES", " on "] {
            assert!(parse_flag("OPTION", value).unwrap());
        }
        for value in ["0", "false", "NO", " off "] {
            assert!(!parse_flag("OPTION", value).unwrap());
        }
        assert!(parse_flag("OPTION", "enabled").is_err());
        assert!(parse_flag("OPTION", "").is_err());
    }

    #[tokio::test]
    async fn supplied_listener_serves_authenticated_api_without_browser_or_real_plugins() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}", listener.local_addr().unwrap());
        let config = RuntimeConfig {
            // Deliberately not the supplied port: serve must not bind this address.
            listen_addr: "127.0.0.1:1".parse().unwrap(),
            database: db::DatabaseConfig::sqlite(":memory:"),
            api_key: "runtime-fixture-key".into(),
            default_channel: crate::messaging::INTERNAL.into(),
            retention_days: 30,
            bot_retention_days: 7,
            artifact_operations: db::ArtifactOperationsEnvelope::default(),
            artifact_body_schema_enabled: true,
            artifact_auth_enforced: false,
            ui_auth_enabled: true,
            #[cfg(feature = "discord")]
            discord: None,
            #[cfg(feature = "whatsapp")]
            whatsapp: None,
        };
        let server = tokio::spawn(serve(config, listener));
        let client = reqwest::Client::builder()
            .timeout(Duration::from_secs(10))
            .build()
            .unwrap();
        let denied = client
            .get(format!("{endpoint}/v1/projects"))
            .send()
            .await
            .unwrap();
        assert_eq!(denied.status(), reqwest::StatusCode::UNAUTHORIZED);
        let fallback = client
            .get(format!("{endpoint}/v1/compat-does-not-exist"))
            .send()
            .await
            .unwrap();
        assert_eq!(fallback.status(), reqwest::StatusCode::UNAUTHORIZED);
        let projects = client
            .get(format!("{endpoint}/v1/projects"))
            .bearer_auth("runtime-fixture-key")
            .send()
            .await
            .unwrap();
        assert_eq!(projects.status(), reqwest::StatusCode::OK);
        assert_eq!(
            projects.json::<serde_json::Value>().await.unwrap(),
            serde_json::json!([])
        );
        let registered = client.post(format!("{endpoint}/v1/projects"))
            .bearer_auth("runtime-fixture-key")
            .json(&serde_json::json!({"ident":"native", "repo_url":"https://github.com/example/native.git"}))
            .send().await.unwrap();
        assert_eq!(registered.status(), reqwest::StatusCode::OK);
        let registered: serde_json::Value = registered.json().await.unwrap();
        assert_eq!(registered["channel_name"], "internal");
        let sent = client
            .post(format!("{endpoint}/v1/projects/native/messages"))
            .bearer_auth("runtime-fixture-key")
            .json(&serde_json::json!({"body":"durable native message"}))
            .send()
            .await
            .unwrap();
        assert_eq!(sent.status(), reqwest::StatusCode::OK);
        let sent: serde_json::Value = sent.json().await.unwrap();
        assert_eq!(
            sent["external_message_id"],
            format!("internal:{}", sent["message_id"])
        );
        let browser = client
            .get(&endpoint)
            .bearer_auth("runtime-fixture-key")
            .send()
            .await
            .unwrap();
        assert_eq!(browser.status(), reqwest::StatusCode::NOT_FOUND);
        #[cfg(feature = "whatsapp")]
        {
            let webhook = client
                .get(format!("{endpoint}/webhooks/whatsapp"))
                .send()
                .await
                .unwrap();
            assert_eq!(webhook.status(), reqwest::StatusCode::SERVICE_UNAVAILABLE);
        }
        server.abort();
        assert!(server.await.unwrap_err().is_cancelled());
    }
}
