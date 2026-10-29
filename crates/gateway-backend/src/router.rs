//! Generic event router (docs/StateMachine.md). It knows nothing about tasks,
//! agents or time beyond retry backoff: it persists events, finds their
//! subscribers and runs each plugin's steps in `depends_on` order.
use crate::db::{self, Db};
use anyhow::{anyhow, bail, Context, Result};
use async_trait::async_trait;
use rusqlite::{params, Connection, OptionalExtension};
use serde_json::{json, Value};
use std::{
    collections::{HashMap, HashSet},
    num::NonZeroU32,
    ops::RangeInclusive,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};
use tokio::{
    sync::Notify,
    task::{spawn_blocking, JoinHandle, JoinSet},
};
use uuid::Uuid;

/// Emitted once when a delivery first reaches the retry cap.
pub const RETRYING: &str = "router.retrying";
const FIRST_RETRY_MS: i64 = 1_000;
const MAX_RETRY_MS: i64 = 30_000;
const MAX_CONCURRENT_DELIVERIES: usize = 64;
const CLAIM_PAGE: i64 = 256;
const TERMINAL: &str = "('completed','stopped','skipped','failed')";
const DELIVERY_SCHEMA: &str = "CREATE TABLE IF NOT EXISTS router_deliveries (
    event_seq       INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,
    plugin          TEXT NOT NULL,
    state           TEXT NOT NULL CHECK(state IN
        ('pending','running','retrying','completed','stopped','skipped','failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY(event_seq, plugin)
);";

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS router_events (
            seq         INTEGER PRIMARY KEY AUTOINCREMENT,
            uid         TEXT NOT NULL UNIQUE,
            id          TEXT NOT NULL,
            version     INTEGER NOT NULL,
            payload     TEXT NOT NULL,
            source      TEXT NOT NULL,
            emitted_at  INTEGER NOT NULL,
            correlation TEXT
        );",
    )?;
    conn.execute_batch(DELIVERY_SCHEMA)?;
    // SQLite cannot extend a CHECK constraint in place. Keep existing delivery
    // progress when upgrading databases created before terminal failures.
    let schema: String = conn.query_row(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name='router_deliveries'",
        [],
        |row| row.get(0),
    )?;
    if !schema.contains("'failed'") {
        let tx = conn.unchecked_transaction()?;
        tx.execute_batch("ALTER TABLE router_deliveries RENAME TO router_deliveries_legacy;")?;
        tx.execute_batch(DELIVERY_SCHEMA)?;
        tx.execute_batch(
            "INSERT INTO router_deliveries SELECT * FROM router_deliveries_legacy;
             DROP TABLE router_deliveries_legacy;",
        )?;
        tx.commit()?;
    }
    conn.execute_batch(
        "CREATE INDEX IF NOT EXISTS idx_router_deliveries_open
        ON router_deliveries(state, next_attempt_at);
        CREATE INDEX IF NOT EXISTS idx_router_deliveries_plugin_open
        ON router_deliveries(plugin, event_seq)
        WHERE state IN ('pending','running','retrying');",
    )?;
    Ok(())
}

#[derive(Debug, Clone, PartialEq)]
pub struct Event {
    pub seq: i64,
    pub id: String,
    pub version: u32,
    pub uid: Uuid,
    pub payload: Value,
    pub source: String,
    pub emitted_at: i64,
    pub correlation: Option<Uuid>,
}

/// One declared event id and payload version.
pub struct EventDefinition {
    pub id: &'static str,
    pub version: u32,
    /// Whether authenticated remote clients may emit it.
    pub remote_emit: bool,
    pub validate: fn(&Value) -> std::result::Result<(), String>,
}

/// The default payload schema: any JSON object.
pub fn object(payload: &Value) -> std::result::Result<(), String> {
    if payload.is_object() {
        Ok(())
    } else {
        Err("payload must be a JSON object".into())
    }
}

pub struct Subscription {
    pub event: &'static str,
    pub versions: RangeInclusive<u32>,
    /// Plugins subscribed to the same event that must complete first.
    pub depends_on: Vec<&'static str>,
}

#[derive(Debug, Clone, PartialEq)]
pub enum StepOutcome {
    Continue,
    /// End this run successfully and skip dependents.
    Stop,
    /// Fail this run; the plugin's retry policy decides whether to run again.
    Fail(String),
}

/// Delivery-level failure policy. Retried runs start again at their first step.
#[derive(Debug, Clone, Copy, Default)]
pub enum RetryPolicy {
    #[default]
    Forever,
    /// Total failed runs allowed, including the initial run. One means no retry.
    MaxAttempts(NonZeroU32),
}

impl RetryPolicy {
    fn exhausted(self, failures: i64) -> bool {
        match self {
            Self::Forever => false,
            Self::MaxAttempts(limit) => failures >= i64::from(limit.get()),
        }
    }
}

#[async_trait]
pub trait Step: Send + Sync {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome;
}

pub trait Plugin: Send + Sync {
    fn id(&self) -> &'static str;
    fn subscriptions(&self) -> Vec<Subscription>;
    /// Process this plugin's deliveries in event sequence order. An unfinished
    /// run, including a retry, holds later events; other plugins remain parallel.
    fn fifo(&self) -> bool {
        false
    }
    fn retry_policy(&self) -> RetryPolicy {
        RetryPolicy::Forever
    }
    fn pre_exec(&self) -> Vec<Box<dyn Step>> {
        Vec::new()
    }
    fn exec(&self) -> Vec<Box<dyn Step>>;
    fn post_exec(&self) -> Vec<Box<dyn Step>> {
        Vec::new()
    }
}

pub struct StepContext {
    bus: Bus,
    plugin: &'static str,
    cause: Uuid,
}

impl StepContext {
    pub fn db(&self) -> &Db {
        &self.bus.db
    }

    /// Emit as this plugin, correlated with the event being handled.
    pub async fn emit(&self, id: &str, version: u32, payload: Value) -> Result<Uuid> {
        let source = format!("plugin:{}", self.plugin);
        self.bus
            .emit(&source, id, version, payload, Some(self.cause))
            .await
    }

    /// [`StepContext::emit`] inside the caller's transaction on `conn`, so the
    /// event commits with the plugin's own changes. Call [`StepContext::wake`]
    /// after committing.
    pub fn emit_in(
        &self,
        conn: &Connection,
        id: &str,
        version: u32,
        payload: Value,
    ) -> Result<Uuid> {
        let source = format!("plugin:{}", self.plugin);
        self.bus
            .emit_in(conn, &source, id, version, payload, Some(self.cause))
    }

    pub fn wake(&self) {
        self.bus.wake();
    }
}

/// A context for driving plugin steps directly in tests.
#[cfg(test)]
pub(crate) fn test_context(bus: &Bus, plugin: &'static str) -> StepContext {
    StepContext {
        bus: bus.clone(),
        plugin,
        cause: Uuid::nil(),
    }
}

struct Registered {
    phases: [Vec<Box<dyn Step>>; 3],
    /// event id -> depends_on
    depends_on: HashMap<&'static str, Vec<&'static str>>,
    retry_policy: RetryPolicy,
    fifo: bool,
}

struct Registry {
    events: HashMap<(String, u32), EventDefinition>,
    plugins: HashMap<&'static str, Registered>,
    subscribers: HashMap<&'static str, Vec<(&'static str, RangeInclusive<u32>)>>,
}

impl Registry {
    fn new(events: Vec<EventDefinition>, plugins: Vec<Box<dyn Plugin>>) -> Result<Self> {
        let mut registry = Registry {
            events: HashMap::new(),
            plugins: HashMap::new(),
            subscribers: HashMap::new(),
        };
        let builtin = EventDefinition {
            id: RETRYING,
            version: 1,
            remote_emit: false,
            validate: object,
        };
        for event in std::iter::once(builtin).chain(events) {
            let key = (event.id.to_owned(), event.version);
            if registry.events.insert(key, event).is_some() {
                bail!("event is declared twice");
            }
        }
        for plugin in plugins {
            let id = plugin.id();
            let mut depends_on = HashMap::new();
            for subscription in plugin.subscriptions() {
                if !registry.events.keys().any(|(event, version)| {
                    event == subscription.event && subscription.versions.contains(version)
                }) {
                    bail!(
                        "plugin {id} subscribes to no declared version of event {}",
                        subscription.event
                    );
                }
                if depends_on
                    .insert(subscription.event, subscription.depends_on)
                    .is_some()
                {
                    bail!("plugin {id} subscribes to {} twice", subscription.event);
                }
                registry
                    .subscribers
                    .entry(subscription.event)
                    .or_default()
                    .push((id, subscription.versions));
            }
            let phases = [plugin.pre_exec(), plugin.exec(), plugin.post_exec()];
            let retry_policy = plugin.retry_policy();
            let fifo = plugin.fifo();
            if registry
                .plugins
                .insert(
                    id,
                    Registered {
                        phases,
                        depends_on,
                        retry_policy,
                        fifo,
                    },
                )
                .is_some()
            {
                bail!("plugin {id} is registered twice");
            }
        }
        registry.validate()?;
        Ok(registry)
    }

    fn validate(&self) -> Result<()> {
        for (event, subscribers) in &self.subscribers {
            for (_, version) in self.events.keys().filter(|(id, _)| id == event) {
                let subscribed: HashSet<_> = subscribers
                    .iter()
                    .filter(|(_, versions)| versions.contains(version))
                    .map(|(plugin, _)| *plugin)
                    .collect();
                for plugin in &subscribed {
                    for dependency in &self.plugins[plugin].depends_on[event] {
                        if !subscribed.contains(dependency) {
                            bail!(
                                "plugin {plugin} depends on {dependency}, which does not subscribe to {event} v{version}"
                            );
                        }
                    }
                }
                let mut done = HashSet::new();
                for plugin in &subscribed {
                    self.acyclic(event, plugin, &mut Vec::new(), &mut done)?;
                }
            }
        }
        Ok(())
    }

    fn acyclic(
        &self,
        event: &'static str,
        plugin: &'static str,
        path: &mut Vec<&'static str>,
        done: &mut HashSet<&'static str>,
    ) -> Result<()> {
        if done.contains(plugin) {
            return Ok(());
        }
        if path.contains(&plugin) {
            bail!(
                "depends_on cycle on {event}: {} -> {plugin}",
                path.join(" -> ")
            );
        }
        path.push(plugin);
        for dependency in &self.plugins[plugin].depends_on[event] {
            self.acyclic(event, dependency, path, done)?;
        }
        path.pop();
        done.insert(plugin);
        Ok(())
    }
}

/// Handle used to emit events; cheap to clone.
#[derive(Clone)]
pub struct Bus {
    db: Db,
    registry: Arc<Registry>,
    wake: Arc<Notify>,
    active: Arc<AtomicBool>,
}

impl Bus {
    /// Persist an event and its deliveries on `conn`, inside any transaction the
    /// caller holds. Call [`Bus::wake`] after the caller commits.
    pub fn emit_in(
        &self,
        conn: &Connection,
        source: &str,
        id: &str,
        version: u32,
        payload: Value,
        correlation: Option<Uuid>,
    ) -> Result<Uuid> {
        let definition = self
            .registry
            .events
            .get(&(id.to_owned(), version))
            .with_context(|| format!("event {id} v{version} is not declared"))?;
        (definition.validate)(&payload)
            .map_err(|error| anyhow!("event {id} v{version}: {error}"))?;
        let uid = Uuid::now_v7();
        let subscribers: Vec<_> = self
            .registry
            .subscribers
            .get(id)
            .into_iter()
            .flatten()
            .filter(|(_, versions)| versions.contains(&version))
            .map(|(plugin, _)| *plugin)
            .collect();
        if subscribers.is_empty() {
            return Ok(uid);
        }
        let now = db::now_ms();
        conn.execute_batch("SAVEPOINT router_emit")?;
        let saved = (|| -> Result<()> {
            conn.execute(
                "INSERT INTO router_events(uid,id,version,payload,source,emitted_at,correlation)
                 VALUES(?1,?2,?3,?4,?5,?6,?7)",
                params![
                    uid.to_string(),
                    id,
                    version,
                    payload.to_string(),
                    source,
                    now,
                    correlation.map(|c| c.to_string())
                ],
            )?;
            let seq = conn.last_insert_rowid();
            for plugin in subscribers {
                conn.execute(
                    "INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at)
                     VALUES(?1,?2,'pending',?3,?3)",
                    params![seq, plugin, now],
                )?;
            }
            Ok(())
        })();
        match saved {
            Ok(()) => conn.execute_batch("RELEASE router_emit")?,
            Err(error) => {
                conn.execute_batch("ROLLBACK TO router_emit; RELEASE router_emit")?;
                return Err(error);
            }
        }
        Ok(uid)
    }

    /// Persist an event and wake the router; returns once it is stored.
    pub async fn emit(
        &self,
        source: &str,
        id: &str,
        version: u32,
        payload: Value,
        correlation: Option<Uuid>,
    ) -> Result<Uuid> {
        let (bus, source, id) = (self.clone(), source.to_owned(), id.to_owned());
        let uid = spawn_blocking(move || {
            let conn = bus.db.lock().unwrap();
            bus.emit_in(&conn, &source, &id, version, payload, correlation)
        })
        .await??;
        self.wake();
        Ok(uid)
    }

    pub fn wake(&self) {
        self.wake.notify_one();
    }

    pub fn db(&self) -> &Db {
        &self.db
    }

    /// Whether a registered plugin consumes this event version.
    pub fn subscribes(&self, plugin: &str, event: &str, version: u32) -> bool {
        self.registry
            .subscribers
            .get(event)
            .is_some_and(|subscribers| {
                subscribers
                    .iter()
                    .any(|(id, versions)| *id == plugin && versions.contains(&version))
            })
    }

    /// Whether authenticated remote clients may emit this event.
    pub fn accepts_remote(&self, id: &str, version: u32) -> bool {
        self.registry
            .events
            .get(&(id.to_owned(), version))
            .is_some_and(|definition| definition.remote_emit)
    }

    /// Mark runnable deliveries running.
    fn claim(&self, now: i64, capacity: usize) -> Result<Claimed> {
        if capacity == 0 {
            return Ok(Claimed {
                runs: Vec::new(),
                next: None,
                skipped: false,
            });
        }
        let conn = self.db.lock().unwrap();
        if !self.active.load(Ordering::SeqCst) {
            return Ok(Claimed {
                runs: Vec::new(),
                next: None,
                skipped: false,
            });
        }
        let tx = conn.unchecked_transaction()?;
        // Cache one indexed head per FIFO plugin. Filtering in SQL prevents a
        // blocked controller's backlog from filling every page of parallel work.
        let mut fifo_heads = serde_json::Map::new();
        let mut parallel = Vec::new();
        for (plugin, registered) in &self.registry.plugins {
            if registered.fifo {
                let head: Option<i64> = tx.query_row(
                    "SELECT MIN(event_seq) FROM router_deliveries WHERE plugin=?1
                     AND state IN ('pending','running','retrying')",
                    params![plugin],
                    |row| row.get(0),
                )?;
                fifo_heads.insert((*plugin).to_owned(), json!(head));
            } else {
                parallel.push(*plugin);
            }
        }
        let fifo_heads = Value::Object(fifo_heads).to_string();
        let parallel = json!(parallel).to_string();
        let (mut runs, mut next, mut skipped) = (Vec::new(), None::<i64>, false);
        let mut after = (0_i64, String::new());
        while runs.len() < capacity {
            let open = {
                let mut statement = tx.prepare(
                    "SELECT d.event_seq, d.plugin, e.id, d.next_attempt_at
                 FROM json_each(?4) fifo
                 JOIN router_deliveries d ON d.plugin=fifo.key AND d.event_seq=fifo.value
                 JOIN router_events e ON e.seq=d.event_seq
                 WHERE d.state IN ('pending','retrying') AND (d.event_seq,d.plugin)>(?1,?2)
                 UNION ALL
                 SELECT d.event_seq, d.plugin, e.id, d.next_attempt_at
                 FROM json_each(?5) parallel
                 CROSS JOIN router_deliveries d INDEXED BY idx_router_deliveries_plugin_open
                 JOIN router_events e ON e.seq=d.event_seq
                 WHERE d.plugin=parallel.value AND d.state IN ('pending','running','retrying')
                 AND d.state!='running' AND (d.event_seq,d.plugin)>(?1,?2)
                 ORDER BY 1, 2 LIMIT ?3",
                )?;
                let rows = statement.query_map(
                    params![after.0, after.1, CLAIM_PAGE, fifo_heads, parallel],
                    |row| {
                        Ok((
                            row.get::<_, i64>(0)?,
                            row.get::<_, String>(1)?,
                            row.get::<_, String>(2)?,
                            row.get::<_, i64>(3)?,
                        ))
                    },
                )?;
                rows.collect::<rusqlite::Result<Vec<_>>>()?
            };
            if open.is_empty() {
                break;
            }
            for (seq, plugin, event, due) in open {
                if runs.len() == capacity {
                    break;
                }
                after = (seq, plugin.clone());
                let Some((plugin, registered)) =
                    self.registry.plugins.get_key_value(plugin.as_str())
                else {
                    continue;
                };
                let (mut ready, mut blocked) = (true, false);
                for dependency in registered
                    .depends_on
                    .get(event.as_str())
                    .into_iter()
                    .flatten()
                {
                    let state: Option<String> = tx
                        .query_row(
                            "SELECT state FROM router_deliveries WHERE event_seq=?1 AND plugin=?2",
                            params![seq, dependency],
                            |row| row.get(0),
                        )
                        .optional()?;
                    match state.as_deref() {
                        Some("completed") => {}
                        None | Some("stopped" | "skipped" | "failed") => blocked = true,
                        _ => ready = false,
                    }
                }
                if blocked {
                    tx.execute(
                    "UPDATE router_deliveries SET state='skipped', last_error='dependency did not complete',
                     updated_at=?3 WHERE event_seq=?1 AND plugin=?2",
                    params![seq, plugin, now],
                )?;
                    skipped = true;
                } else if ready && due > now {
                    next = Some(next.map_or(due, |next| next.min(due)));
                } else if ready {
                    tx.execute(
                        "UPDATE router_deliveries SET state='running', updated_at=?3
                     WHERE event_seq=?1 AND plugin=?2",
                        params![seq, plugin, now],
                    )?;
                    runs.push((load(&tx, seq)?, *plugin));
                }
            }
        }
        tx.commit()?;
        Ok(Claimed {
            runs,
            next,
            skipped,
        })
    }

    fn finish(
        &self,
        conn: &Connection,
        seq: i64,
        plugin: &str,
        outcome: StepOutcome,
        now: i64,
    ) -> Result<()> {
        let state = match outcome {
            StepOutcome::Continue => "completed",
            StepOutcome::Stop => "stopped",
            StepOutcome::Fail(error) => {
                let attempts: i64 = conn.query_row(
                    "SELECT attempts + 1 FROM router_deliveries WHERE event_seq=?1 AND plugin=?2",
                    params![seq, plugin],
                    |row| row.get(0),
                )?;
                let delay = retry_delay(attempts);
                if self.registry.plugins[plugin]
                    .retry_policy
                    .exhausted(attempts)
                {
                    conn.execute(
                        "UPDATE router_deliveries SET state='failed', attempts=?3,
                         last_error=?4, updated_at=?5 WHERE event_seq=?1 AND plugin=?2",
                        params![seq, plugin, attempts, error, now],
                    )?;
                    return Ok(());
                }
                conn.execute(
                    "UPDATE router_deliveries SET state='retrying', attempts=?3, next_attempt_at=?4,
                     last_error=?5, updated_at=?6 WHERE event_seq=?1 AND plugin=?2",
                    params![seq, plugin, attempts, now + delay, error, now],
                )?;
                let event = load(conn, seq)?;
                if delay == MAX_RETRY_MS
                    && retry_delay(attempts - 1) < MAX_RETRY_MS
                    && event.id != RETRYING
                {
                    let payload = json!({
                        "event_uid": event.uid.to_string(), "event": event.id, "plugin": plugin,
                        "attempts": attempts, "error": error,
                    });
                    self.emit_in(conn, "router", RETRYING, 1, payload, Some(event.uid))?;
                }
                return Ok(());
            }
        };
        conn.execute(
            "UPDATE router_deliveries SET state=?3, last_error=NULL, updated_at=?4
             WHERE event_seq=?1 AND plugin=?2",
            params![seq, plugin, state, now],
        )?;
        Ok(())
    }
}

struct Claimed {
    runs: Vec<(Event, &'static str)>,
    /// The earliest future retry.
    next: Option<i64>,
    /// A skip may unblock other deliveries, so claim again.
    skipped: bool,
}

/// Backoff after the given number of failed runs: 1 s, doubling, capped at 30 s.
pub fn retry_delay(failures: i64) -> i64 {
    if failures <= 0 {
        return 0;
    }
    (FIRST_RETRY_MS << (failures - 1).min(5)).min(MAX_RETRY_MS)
}

pub(crate) fn load(conn: &Connection, seq: i64) -> Result<Event> {
    let row = conn.query_row(
        "SELECT uid,id,version,payload,source,emitted_at,correlation FROM router_events WHERE seq=?1",
        params![seq],
        |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, u32>(2)?,
                row.get::<_, String>(3)?,
                row.get::<_, String>(4)?,
                row.get::<_, i64>(5)?,
                row.get::<_, Option<String>>(6)?,
            ))
        },
    )?;
    Ok(Event {
        seq,
        uid: row.0.parse()?,
        id: row.1,
        version: row.2,
        payload: serde_json::from_str(&row.3)?,
        source: row.4,
        emitted_at: row.5,
        correlation: row.6.map(|c| c.parse()).transpose()?,
    })
}

/// Validate the registry, replay unfinished work and start the router.
pub fn start(
    db: Db,
    events: Vec<EventDefinition>,
    plugins: Vec<Box<dyn Plugin>>,
) -> Result<(Bus, tokio::task::JoinHandle<()>)> {
    let registry = Arc::new(Registry::new(events, plugins)?);
    recover(&db.lock().unwrap(), &registry)?;
    let bus = Bus {
        db,
        registry,
        wake: Arc::new(Notify::new()),
        active: Arc::new(AtomicBool::new(true)),
    };
    Ok((bus.clone(), tokio::spawn(route(bus))))
}

/// Runs interrupted by a restart run again; deliveries whose subscription was
/// removed can never run, so they are skipped.
fn recover(conn: &Connection, registry: &Registry) -> Result<()> {
    let tx = conn.unchecked_transaction()?;
    let now = db::now_ms();
    tx.execute(
        "UPDATE router_deliveries SET state='pending', updated_at=?1 WHERE state='running'",
        params![now],
    )?;
    let deliveries = tx
        .prepare(&format!(
            "SELECT d.event_seq, d.plugin, e.id, e.version FROM router_deliveries d
             JOIN router_events e ON e.seq=d.event_seq WHERE d.state NOT IN {TERMINAL}"
        ))?
        .query_map([], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, String>(2)?,
                row.get::<_, u32>(3)?,
            ))
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for (seq, plugin, event, version) in deliveries {
        let subscribed = registry
            .subscribers
            .get(event.as_str())
            .is_some_and(|subscribers| {
                subscribers
                    .iter()
                    .any(|(id, versions)| *id == plugin && versions.contains(&version))
            });
        if !subscribed {
            tx.execute(
                "UPDATE router_deliveries SET state='skipped', last_error='subscription is not registered',
                 updated_at=?3 WHERE event_seq=?1 AND plugin=?2",
                params![seq, plugin, now],
            )?;
        }
    }
    tx.commit()?;
    Ok(())
}

async fn route(bus: Bus) {
    // Own executions so shutting down the router also stops its controllers.
    let mut workers = JoinSet::new();
    let _lifetime = RouterLifetime(bus.active.clone());
    loop {
        let claimer = bus.clone();
        let capacity = MAX_CONCURRENT_DELIVERIES - workers.len();
        let next = match spawn_blocking(move || claimer.claim(db::now_ms(), capacity)).await {
            Ok(Ok(Claimed {
                runs,
                next,
                skipped,
            })) => {
                for (event, plugin) in runs {
                    workers.spawn(execute(bus.clone(), event, plugin));
                }
                if skipped {
                    bus.wake();
                }
                next
            }
            Ok(Err(error)) => {
                tracing::warn!("Event router claim failed: {error}");
                Some(db::now_ms() + FIRST_RETRY_MS)
            }
            Err(error) => {
                tracing::warn!("Event router claim task failed: {error}");
                Some(db::now_ms() + FIRST_RETRY_MS)
            }
        };
        let wait = next.map_or(Duration::from_secs(3600), |at| {
            Duration::from_millis((at - db::now_ms()).max(0) as u64)
        });
        tokio::select! {
            _ = workers.join_next(), if !workers.is_empty() => {}
            _ = bus.wake.notified() => {}
            _ = tokio::time::sleep(wait) => {}
        }
    }
}

struct RouterLifetime(Arc<AtomicBool>);

impl Drop for RouterLifetime {
    fn drop(&mut self) {
        // Blocking work queued on the DB mutex can outlive task cancellation.
        self.0.store(false, Ordering::SeqCst);
    }
}

/// A plugin task must not outlive its owning delivery when the router stops.
struct PluginRun(JoinHandle<StepOutcome>);

impl Drop for PluginRun {
    fn drop(&mut self) {
        self.0.abort();
    }
}

async fn execute(bus: Bus, event: Event, plugin: &'static str) {
    let seq = event.seq;
    let ctx = StepContext {
        bus: bus.clone(),
        plugin,
        cause: event.uid,
    };
    let registry = bus.registry.clone();
    let mut run = PluginRun(tokio::spawn(async move {
        for phase in &registry.plugins[plugin].phases {
            for step in phase {
                match step.run(&event, &ctx).await {
                    StepOutcome::Continue => {}
                    outcome => return outcome,
                }
            }
        }
        StepOutcome::Continue
    }));
    let outcome = (&mut run.0)
        .await
        .unwrap_or_else(|error| StepOutcome::Fail(format!("plugin panicked: {error}")));
    // A transient recording failure must not strand a delivery in `running`
    // until restart or repeat its already-completed side effects.
    loop {
        let finisher = bus.clone();
        let outcome = outcome.clone();
        let finished = spawn_blocking(move || {
            let conn = finisher.db.lock().unwrap();
            if !finisher.active.load(Ordering::SeqCst) {
                return anyhow::Ok(());
            }
            let tx = conn.unchecked_transaction()?;
            finisher.finish(&tx, seq, plugin, outcome, db::now_ms())?;
            tx.commit()?;
            anyhow::Ok(())
        })
        .await;
        match finished {
            Ok(Ok(())) => break,
            Ok(Err(error)) => {
                tracing::warn!("Event router could not record {plugin} for event {seq}: {error}")
            }
            Err(error) => tracing::warn!("Event router record task failed: {error}"),
        }
        tokio::time::sleep(Duration::from_millis(FIRST_RETRY_MS as u64)).await;
    }
    bus.wake();
}

/// Remove finished events older than `cutoff_ms`, through an optional external
/// retention boundary. The caller owns any outstanding delivery obligations.
pub fn prune(conn: &Connection, cutoff_ms: i64, through_seq: Option<i64>) -> Result<usize> {
    Ok(conn.execute(
        &format!(
            "DELETE FROM router_events WHERE emitted_at < ?1 AND (?2 IS NULL OR seq<=?2) AND NOT EXISTS (
             SELECT 1 FROM router_deliveries d WHERE d.event_seq = router_events.seq
             AND d.state NOT IN {TERMINAL})"
        ),
        params![cutoff_ms, through_seq],
    )?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        atomic::{AtomicBool, AtomicUsize, Ordering},
        Mutex,
    };
    use std::time::Instant;
    use tokio::sync::Barrier;

    const EVENT: &str = "test.event";
    const OTHER: &str = "test.other";
    type Log = Arc<Mutex<Vec<String>>>;

    #[derive(Clone)]
    struct Record {
        name: &'static str,
        log: Log,
        /// Outcomes returned on successive runs; `Continue` once exhausted.
        outcomes: Arc<Mutex<Vec<StepOutcome>>>,
        delay: Duration,
        barrier: Option<Arc<Barrier>>,
    }

    fn record(name: &'static str, log: &Log) -> Record {
        Record {
            name,
            log: log.clone(),
            outcomes: Arc::default(),
            delay: Duration::ZERO,
            barrier: None,
        }
    }

    impl Record {
        fn then(self, outcomes: Vec<StepOutcome>) -> Self {
            *self.outcomes.lock().unwrap() = outcomes.into_iter().rev().collect();
            self
        }
    }

    #[async_trait]
    impl Step for Record {
        async fn run(&self, _event: &Event, _ctx: &StepContext) -> StepOutcome {
            if let Some(barrier) = &self.barrier {
                barrier.wait().await;
            }
            tokio::time::sleep(self.delay).await;
            self.log.lock().unwrap().push(self.name.into());
            self.outcomes
                .lock()
                .unwrap()
                .pop()
                .unwrap_or(StepOutcome::Continue)
        }
    }

    struct Panics(AtomicBool);

    #[async_trait]
    impl Step for Panics {
        async fn run(&self, _event: &Event, _ctx: &StepContext) -> StepOutcome {
            if !self.0.swap(true, Ordering::SeqCst) {
                panic!("first run panics");
            }
            StepOutcome::Continue
        }
    }

    struct TestPlugin {
        id: &'static str,
        event: &'static str,
        depends_on: Vec<&'static str>,
        pre: Vec<Record>,
        exec: Vec<Record>,
        post: Vec<Record>,
        retry_policy: RetryPolicy,
        versions: RangeInclusive<u32>,
        fifo: bool,
    }

    fn plugin(id: &'static str, depends_on: &[&'static str], exec: Vec<Record>) -> TestPlugin {
        TestPlugin {
            id,
            event: EVENT,
            depends_on: depends_on.to_vec(),
            pre: Vec::new(),
            exec,
            post: Vec::new(),
            retry_policy: RetryPolicy::Forever,
            versions: 1..=1,
            fifo: false,
        }
    }

    fn boxed(steps: &[Record]) -> Vec<Box<dyn Step>> {
        steps
            .iter()
            .cloned()
            .map(|step| Box::new(step) as Box<dyn Step>)
            .collect()
    }

    impl Plugin for TestPlugin {
        fn id(&self) -> &'static str {
            self.id
        }
        fn subscriptions(&self) -> Vec<Subscription> {
            vec![Subscription {
                event: self.event,
                versions: self.versions.clone(),
                depends_on: self.depends_on.clone(),
            }]
        }
        fn pre_exec(&self) -> Vec<Box<dyn Step>> {
            boxed(&self.pre)
        }
        fn retry_policy(&self) -> RetryPolicy {
            self.retry_policy
        }
        fn fifo(&self) -> bool {
            self.fifo
        }
        fn exec(&self) -> Vec<Box<dyn Step>> {
            boxed(&self.exec)
        }
        fn post_exec(&self) -> Vec<Box<dyn Step>> {
            boxed(&self.post)
        }
    }

    fn events() -> Vec<EventDefinition> {
        [EVENT, OTHER]
            .into_iter()
            .map(|id| EventDefinition {
                id,
                version: 1,
                remote_emit: false,
                validate: object,
            })
            .collect()
    }

    fn boxes(plugins: Vec<TestPlugin>) -> Vec<Box<dyn Plugin>> {
        plugins
            .into_iter()
            .map(|plugin| Box::new(plugin) as Box<dyn Plugin>)
            .collect()
    }

    /// A bus whose router is not running, for driving claim/finish directly.
    fn idle_bus(db: Db, plugins: Vec<Box<dyn Plugin>>) -> Bus {
        Bus {
            db,
            registry: Arc::new(Registry::new(events(), plugins).unwrap()),
            wake: Arc::new(Notify::new()),
            active: Arc::new(AtomicBool::new(true)),
        }
    }

    fn states(db: &Db) -> Vec<(String, String, i64)> {
        let conn = db.lock().unwrap();
        let mut statement = conn
            .prepare(
                "SELECT plugin, state, attempts FROM router_deliveries ORDER BY event_seq, plugin",
            )
            .unwrap();
        statement
            .query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap()
    }

    fn count(db: &Db, sql: &str) -> i64 {
        db.lock()
            .unwrap()
            .query_row(sql, [], |row| row.get(0))
            .unwrap()
    }

    async fn settle(db: &Db) {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            let open = count(
                db,
                &format!("SELECT COUNT(*) FROM router_deliveries WHERE state NOT IN {TERMINAL}"),
            );
            if open == 0 {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "deliveries never settled: {:?}",
                states(db)
            );
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }

    fn logged(log: &Log) -> Vec<String> {
        log.lock().unwrap().clone()
    }

    #[tokio::test]
    async fn events_without_subscribers_are_skipped_and_undeclared_or_invalid_events_rejected() {
        let db = db::open(":memory:").unwrap();
        let (bus, _router) = start(db.clone(), events(), Vec::new()).unwrap();
        bus.emit("test", EVENT, 1, json!({}), None).await.unwrap();
        assert_eq!(count(&db, "SELECT COUNT(*) FROM router_events"), 0);
        assert!(bus
            .emit("test", "test.missing", 1, json!({}), None)
            .await
            .is_err());
        assert!(bus.emit("test", EVENT, 2, json!({}), None).await.is_err());
        assert!(bus.emit("test", EVENT, 1, json!(1), None).await.is_err());
    }

    #[tokio::test]
    async fn independent_plugins_run_in_parallel() {
        let db = db::open(":memory:").unwrap();
        let log = Log::default();
        let barrier = Arc::new(Barrier::new(2));
        let step = |name| Record {
            barrier: Some(barrier.clone()),
            ..record(name, &log)
        };
        let plugins = vec![
            plugin("a", &[], vec![step("a")]),
            plugin("b", &[], vec![step("b")]),
        ];
        let (bus, _router) = start(db.clone(), events(), boxes(plugins)).unwrap();
        bus.emit("test", EVENT, 1, json!({}), None).await.unwrap();
        // Each step waits for the other, so this only settles if both run at once.
        settle(&db).await;
        assert_eq!(
            states(&db)
                .iter()
                .filter(|(_, state, _)| state == "completed")
                .count(),
            2
        );
    }

    #[tokio::test]
    async fn dependents_wait_for_completion_and_steps_run_in_phase_order() {
        let db = db::open(":memory:").unwrap();
        let log = Log::default();
        let slow = Record {
            delay: Duration::from_millis(150),
            ..record("a.exec", &log)
        };
        let a = TestPlugin {
            pre: vec![record("a.pre", &log)],
            post: vec![record("a.post", &log)],
            ..plugin("a", &[], vec![slow, record("a.exec2", &log)])
        };
        let plugins = vec![
            a,
            plugin("b", &["a"], vec![record("b.exec", &log)]),
            plugin("c", &[], vec![record("c.exec", &log)]),
        ];
        let (bus, _router) = start(db.clone(), events(), boxes(plugins)).unwrap();
        bus.emit("test", EVENT, 1, json!({}), None).await.unwrap();
        settle(&db).await;
        let log = logged(&log);
        let at = |name: &str| log.iter().position(|entry| entry == name).unwrap();
        assert!(
            at("a.pre") < at("a.exec")
                && at("a.exec") < at("a.exec2")
                && at("a.exec2") < at("a.post")
        );
        assert!(at("a.post") < at("b.exec"), "{log:?}");
        assert!(
            at("c.exec") < at("a.exec"),
            "c has no depends_on and must not wait: {log:?}"
        );
    }

    #[tokio::test]
    async fn stop_ends_the_run_and_skips_only_its_dependents() {
        let db = db::open(":memory:").unwrap();
        let log = Log::default();
        let a = TestPlugin {
            pre: vec![record("a.pre", &log).then(vec![StepOutcome::Stop])],
            ..plugin("a", &[], vec![record("a.exec", &log)])
        };
        let plugins = vec![
            a,
            plugin("b", &["a"], vec![record("b.exec", &log)]),
            plugin("c", &[], vec![record("c.exec", &log)]),
        ];
        let (bus, _router) = start(db.clone(), events(), boxes(plugins)).unwrap();
        bus.emit("test", EVENT, 1, json!({}), None).await.unwrap();
        settle(&db).await;
        let mut log = logged(&log);
        log.sort();
        assert_eq!(log, ["a.pre", "c.exec"]);
        let states: Vec<_> = states(&db).into_iter().map(|(_, state, _)| state).collect();
        assert_eq!(states, ["stopped", "skipped", "completed"]);
    }

    #[tokio::test]
    async fn failed_runs_retry_from_the_first_step_after_backoff_and_hold_dependents() {
        let db = db::open(":memory:").unwrap();
        let log = Log::default();
        let a = TestPlugin {
            pre: vec![record("a.pre", &log)],
            ..plugin(
                "a",
                &[],
                vec![record("a.exec", &log).then(vec![StepOutcome::Fail("boom".into())])],
            )
        };
        let plugins = vec![a, plugin("b", &["a"], vec![record("b.exec", &log)])];
        let (bus, _router) = start(db.clone(), events(), boxes(plugins)).unwrap();
        let started = Instant::now();
        bus.emit("test", EVENT, 1, json!({}), None).await.unwrap();
        settle(&db).await;
        assert!(started.elapsed() >= Duration::from_secs(1));
        assert_eq!(
            logged(&log),
            ["a.pre", "a.exec", "a.pre", "a.exec", "b.exec"]
        );
        assert_eq!(
            states(&db),
            [
                ("a".into(), "completed".into(), 1),
                ("b".into(), "completed".into(), 0)
            ]
        );
    }

    #[tokio::test]
    async fn a_panicking_step_is_retried_like_a_failure() {
        struct Panicky;
        impl Plugin for Panicky {
            fn id(&self) -> &'static str {
                "panicky"
            }
            fn subscriptions(&self) -> Vec<Subscription> {
                vec![Subscription {
                    event: EVENT,
                    versions: 1..=1,
                    depends_on: Vec::new(),
                }]
            }
            fn exec(&self) -> Vec<Box<dyn Step>> {
                vec![Box::new(Panics(AtomicBool::new(false)))]
            }
        }
        let db = db::open(":memory:").unwrap();
        let (bus, _router) = start(db.clone(), events(), vec![Box::new(Panicky)]).unwrap();
        bus.emit("test", EVENT, 1, json!({}), None).await.unwrap();
        settle(&db).await;
        assert_eq!(states(&db), [("panicky".into(), "completed".into(), 1)]);
    }

    #[test]
    fn retry_delay_doubles_from_one_second_and_caps_at_thirty() {
        let delays: Vec<_> = (0..9).map(retry_delay).collect();
        assert_eq!(
            delays,
            [0, 1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000]
        );
        assert_eq!(retry_delay(i64::MAX), 30_000);
    }

    #[test]
    fn paged_claims_reach_runnable_work_after_delayed_backlog_and_respect_capacity() {
        let db = db::open(":memory:").unwrap();
        let bus = idle_bus(db.clone(), boxes(vec![plugin("a", &[], Vec::new())]));
        let total = CLAIM_PAGE + 3;
        let now = {
            let conn = db.lock().unwrap();
            let tx = conn.unchecked_transaction().unwrap();
            for _ in 0..total {
                bus.emit_in(&tx, "test", EVENT, 1, json!({}), None).unwrap();
            }
            let now = db::now_ms();
            tx.execute(
                "UPDATE router_deliveries SET next_attempt_at=?1 WHERE event_seq<=?2",
                params![now + 10_000, CLAIM_PAGE],
            )
            .unwrap();
            tx.commit().unwrap();
            now
        };
        let claimed = bus.claim(now + 100, 2).unwrap();
        assert_eq!(
            claimed
                .runs
                .iter()
                .map(|(event, _)| event.seq)
                .collect::<Vec<_>>(),
            [CLAIM_PAGE + 1, CLAIM_PAGE + 2]
        );
        assert_eq!(claimed.next, Some(now + 10_000));
        let claimed = bus.claim(now + 100, 2).unwrap();
        assert_eq!(claimed.runs[0].0.seq, total);
        assert_eq!(claimed.runs.len(), 1);
        assert_eq!(claimed.next, Some(now + 10_000));
        assert!(bus.claim(now + 100, 2).unwrap().runs.is_empty());
        assert_eq!(bus.claim(now + 10_000, 2).unwrap().runs.len(), 2);
        assert_eq!(
            count(
                &db,
                "SELECT COUNT(*) FROM router_deliveries WHERE state='pending'"
            ),
            CLAIM_PAGE - 2
        );
    }

    #[test]
    fn fifo_holds_running_and_retrying_heads_without_blocking_parallel_work() {
        let db = db::open(":memory:").unwrap();
        let serial = TestPlugin {
            fifo: true,
            ..plugin("serial", &[], Vec::new())
        };
        let parallel = TestPlugin {
            event: OTHER,
            ..plugin("parallel", &[], Vec::new())
        };
        let bus = idle_bus(db.clone(), boxes(vec![serial, parallel]));
        let now = db::now_ms();
        {
            let conn = db.lock().unwrap();
            let tx = conn.unchecked_transaction().unwrap();
            for _ in 0..CLAIM_PAGE + 32 {
                bus.emit_in(&tx, "test", EVENT, 1, json!({}), None).unwrap();
            }
            bus.emit_in(&tx, "test", OTHER, 1, json!({}), None).unwrap();
            tx.commit().unwrap();
        }
        let claimed = bus.claim(now + 1000, 2).unwrap();
        assert_eq!(
            claimed
                .runs
                .iter()
                .map(|(event, plugin)| (event.seq, *plugin))
                .collect::<Vec<_>>(),
            [(1, "serial"), (CLAIM_PAGE + 33, "parallel")]
        );
        assert!(
            bus.claim(now + 1000, 2).unwrap().runs.is_empty(),
            "a running head holds later FIFO input"
        );
        bus.finish(
            &db.lock().unwrap(),
            1,
            "serial",
            StepOutcome::Fail("transient".into()),
            now + 1000,
        )
        .unwrap();
        let waiting = bus.claim(now + 1000, 2).unwrap();
        assert!(waiting.runs.is_empty());
        assert_eq!(waiting.next, Some(now + 2000));
        let retry = bus.claim(now + 2000, 2).unwrap();
        assert_eq!(
            retry
                .runs
                .iter()
                .map(|(event, _)| event.seq)
                .collect::<Vec<_>>(),
            [1]
        );
        bus.finish(
            &db.lock().unwrap(),
            1,
            "serial",
            StepOutcome::Continue,
            now + 2000,
        )
        .unwrap();
        let next = bus.claim(now + 2000, 2).unwrap();
        assert_eq!(
            next.runs
                .iter()
                .map(|(event, _)| event.seq)
                .collect::<Vec<_>>(),
            [2]
        );
    }

    #[test]
    fn bounded_failures_settle_dependents_and_survive_recovery() {
        for limit in [1, 3] {
            let db = db::open(":memory:").unwrap();
            let limited = TestPlugin {
                retry_policy: RetryPolicy::MaxAttempts(NonZeroU32::new(limit).unwrap()),
                ..plugin("a", &[], Vec::new())
            };
            let bus = idle_bus(
                db.clone(),
                boxes(vec![
                    limited,
                    plugin("b", &["a"], Vec::new()),
                    plugin("c", &[], Vec::new()),
                ]),
            );
            bus.emit_in(&db.lock().unwrap(), "test", EVENT, 1, json!({}), None)
                .unwrap();
            assert_eq!(
                bus.claim(0, MAX_CONCURRENT_DELIVERIES).unwrap().runs.len(),
                0
            ); // Not due yet.
            let mut now = db::now_ms();
            assert_eq!(
                bus.claim(now, MAX_CONCURRENT_DELIVERIES)
                    .unwrap()
                    .runs
                    .len(),
                2
            );
            bus.finish(&db.lock().unwrap(), 1, "c", StepOutcome::Continue, now)
                .unwrap();
            for attempt in 1..=limit {
                bus.finish(
                    &db.lock().unwrap(),
                    1,
                    "a",
                    StepOutcome::Fail("broken".into()),
                    now,
                )
                .unwrap();
                if attempt < limit {
                    assert!(bus
                        .claim(now, MAX_CONCURRENT_DELIVERIES)
                        .unwrap()
                        .runs
                        .is_empty());
                    now += retry_delay(i64::from(attempt));
                    assert_eq!(
                        bus.claim(now, MAX_CONCURRENT_DELIVERIES)
                            .unwrap()
                            .runs
                            .len(),
                        1
                    );
                }
            }
            assert!(bus
                .claim(now, MAX_CONCURRENT_DELIVERIES)
                .unwrap()
                .runs
                .is_empty());
            assert_eq!(
                states(&db),
                [
                    ("a".into(), "failed".into(), i64::from(limit)),
                    ("b".into(), "skipped".into(), 0),
                    ("c".into(), "completed".into(), 0),
                ]
            );
            let conn = db.lock().unwrap();
            let error: String = conn
                .query_row(
                    "SELECT last_error FROM router_deliveries WHERE plugin='a'",
                    [],
                    |r| r.get(0),
                )
                .unwrap();
            assert_eq!(error, "broken");
            recover(&conn, &bus.registry).unwrap();
            drop(conn);
            assert!(bus
                .claim(i64::MAX, MAX_CONCURRENT_DELIVERIES)
                .unwrap()
                .runs
                .is_empty());
            assert_eq!(prune(&db.lock().unwrap(), i64::MAX, None).unwrap(), 1);
        }
    }

    #[test]
    fn upgrade_preserves_legacy_delivery_progress_and_foreign_keys() {
        let db = db::open(":memory:").unwrap();
        let bus = idle_bus(db.clone(), boxes(vec![plugin("a", &[], Vec::new())]));
        let conn = db.lock().unwrap();
        let uid = bus
            .emit_in(&conn, "test", EVENT, 1, json!({}), None)
            .unwrap();
        // Reproduce the previous schema without its new terminal state.
        conn.execute_batch("ALTER TABLE router_deliveries RENAME TO saved_deliveries;
            CREATE TABLE router_deliveries (
                event_seq INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,
                plugin TEXT NOT NULL,
                state TEXT NOT NULL CHECK(state IN ('pending','running','retrying','completed','stopped','skipped')),
                attempts INTEGER NOT NULL DEFAULT 0,
                next_attempt_at INTEGER NOT NULL, last_error TEXT, updated_at INTEGER NOT NULL,
                PRIMARY KEY(event_seq, plugin));
            INSERT INTO router_deliveries SELECT * FROM saved_deliveries;
            DROP TABLE saved_deliveries;
            UPDATE router_deliveries SET state='retrying', attempts=2, last_error='offline';").unwrap();
        initialize(&conn).unwrap();
        initialize(&conn).unwrap();
        assert_eq!(load(&conn, 1).unwrap().uid, uid);
        let progress: (String, i64, String) = conn
            .query_row(
                "SELECT state, attempts, last_error FROM router_deliveries",
                [],
                |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
            )
            .unwrap();
        assert_eq!(progress, ("retrying".into(), 2, "offline".into()));
        conn.execute("UPDATE router_deliveries SET state='failed'", [])
            .unwrap();
        conn.execute("DELETE FROM router_events", []).unwrap();
        let remaining: i64 = conn
            .query_row("SELECT COUNT(*) FROM router_deliveries", [], |r| r.get(0))
            .unwrap();
        assert_eq!(remaining, 0);
    }

    #[test]
    fn dependencies_must_cover_each_subscribed_event_version() {
        let definitions = || {
            let mut definitions = events();
            definitions.push(EventDefinition {
                id: EVENT,
                version: 2,
                remote_emit: false,
                validate: object,
            });
            definitions
        };
        let a = || plugin("a", &[], Vec::new());
        let b = || TestPlugin {
            versions: 2..=2,
            ..plugin("b", &["a"], Vec::new())
        };
        assert!(Registry::new(definitions(), boxes(vec![a(), b()])).is_err());
        let a = TestPlugin {
            versions: 1..=2,
            ..a()
        };
        assert!(Registry::new(definitions(), boxes(vec![a, b()])).is_ok());
        let unavailable = TestPlugin {
            versions: 3..=3,
            ..plugin("x", &[], Vec::new())
        };
        assert!(Registry::new(definitions(), boxes(vec![unavailable])).is_err());
    }

    #[test]
    fn recovery_skips_removed_subscriptions_on_registered_plugins() {
        let db = db::open(":memory:").unwrap();
        let bus = idle_bus(db.clone(), boxes(vec![plugin("a", &[], Vec::new())]));
        bus.emit_in(&db.lock().unwrap(), "test", EVENT, 1, json!({}), None)
            .unwrap();
        let changed = TestPlugin {
            event: OTHER,
            ..plugin("a", &[], Vec::new())
        };
        let registry = Registry::new(events(), boxes(vec![changed])).unwrap();
        recover(&db.lock().unwrap(), &registry).unwrap();
        assert_eq!(states(&db), [("a".into(), "skipped".into(), 0)]);
    }

    #[tokio::test]
    async fn recording_failure_retries_without_reexecuting_plugin_steps() {
        let db = db::open(":memory:").unwrap();
        let log = Log::default();
        let bus = idle_bus(
            db.clone(),
            boxes(vec![plugin("a", &[], vec![record("a", &log)])]),
        );
        let event = {
            let conn = db.lock().unwrap();
            bus.emit_in(&conn, "test", EVENT, 1, json!({}), None)
                .unwrap();
            conn.execute_batch(
                "CREATE TRIGGER reject_completion BEFORE UPDATE ON router_deliveries
                WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT, 'disk unavailable'); END;",
            )
            .unwrap();
            load(&conn, 1).unwrap()
        };
        bus.claim(db::now_ms(), 1).unwrap();
        let run = tokio::spawn(execute(bus.clone(), event, "a"));
        let deadline = Instant::now() + Duration::from_secs(5);
        while logged(&log).is_empty() {
            assert!(Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        // Give the first completion write a chance to fail.
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(states(&db), [("a".into(), "running".into(), 0)]);
        db.lock()
            .unwrap()
            .execute_batch("DROP TRIGGER reject_completion")
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), run)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(logged(&log), ["a"]);
        assert_eq!(states(&db), [("a".into(), "completed".into(), 0)]);
    }

    #[tokio::test]
    async fn router_shutdown_cancels_owned_controller_runs_and_bounds_concurrency() {
        struct Waiting(Arc<AtomicUsize>);
        struct Active(Arc<AtomicUsize>);
        impl Drop for Active {
            fn drop(&mut self) {
                self.0.fetch_sub(1, Ordering::SeqCst);
            }
        }
        #[async_trait]
        impl Step for Waiting {
            async fn run(&self, _: &Event, _: &StepContext) -> StepOutcome {
                let _active = Active(self.0.clone());
                self.0.fetch_add(1, Ordering::SeqCst);
                std::future::pending().await
            }
        }
        struct Controller(Arc<AtomicUsize>);
        impl Plugin for Controller {
            fn id(&self) -> &'static str {
                "waiting"
            }
            fn subscriptions(&self) -> Vec<Subscription> {
                vec![Subscription {
                    event: EVENT,
                    versions: 1..=1,
                    depends_on: Vec::new(),
                }]
            }
            fn exec(&self) -> Vec<Box<dyn Step>> {
                vec![Box::new(Waiting(self.0.clone()))]
            }
        }
        let db = db::open(":memory:").unwrap();
        let active = Arc::new(AtomicUsize::new(0));
        let (bus, router) = start(
            db.clone(),
            events(),
            vec![Box::new(Controller(active.clone()))],
        )
        .unwrap();
        for _ in 0..MAX_CONCURRENT_DELIVERIES + 3 {
            bus.emit("test", EVENT, 1, json!({}), None).await.unwrap();
        }
        let deadline = Instant::now() + Duration::from_secs(5);
        while count(
            &db,
            "SELECT COUNT(*) FROM router_deliveries WHERE state='running'",
        ) < MAX_CONCURRENT_DELIVERIES as i64
        {
            assert!(Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(
            count(
                &db,
                "SELECT COUNT(*) FROM router_deliveries WHERE state='pending'"
            ),
            3
        );
        assert!(active.load(Ordering::SeqCst) > 0);
        router.abort();
        let _ = router.await;
        while active.load(Ordering::SeqCst) > 0 {
            assert!(Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        // Interrupted deliveries remain durable for recovery.
        assert_eq!(
            count(
                &db,
                "SELECT COUNT(*) FROM router_deliveries WHERE state='running'"
            ),
            MAX_CONCURRENT_DELIVERIES as i64
        );
        assert!(bus
            .claim(i64::MAX, MAX_CONCURRENT_DELIVERIES)
            .unwrap()
            .runs
            .is_empty());
    }

    #[tokio::test]
    async fn queued_claim_cannot_mutate_deliveries_after_router_shutdown() {
        let db = db::open(":memory:").unwrap();
        let bus = idle_bus(db.clone(), boxes(vec![plugin("a", &[], Vec::new())]));
        let pending = {
            let conn = db.lock().unwrap();
            bus.emit_in(&conn, "test", EVENT, 1, json!({}), None)
                .unwrap();
            let claimer = bus.clone();
            let pending = spawn_blocking(move || claimer.claim(i64::MAX, 1));
            drop(RouterLifetime(bus.active.clone()));
            pending
        };
        assert!(pending.await.unwrap().unwrap().runs.is_empty());
        assert_eq!(states(&db), [("a".into(), "pending".into(), 0)]);
    }

    #[test]
    fn reaching_the_retry_cap_emits_router_retrying_once() {
        let db = db::open(":memory:").unwrap();
        let log = Log::default();
        let watcher = TestPlugin {
            event: RETRYING,
            ..plugin("watcher", &[], vec![record("watched", &log)])
        };
        let bus = idle_bus(
            db.clone(),
            boxes(vec![plugin("a", &[], Vec::new()), watcher]),
        );
        let conn = db.lock().unwrap();
        let uid = bus
            .emit_in(&conn, "test", EVENT, 1, json!({}), None)
            .unwrap();
        conn.execute("UPDATE router_deliveries SET attempts=4", [])
            .unwrap();
        let retrying = "SELECT COUNT(*) FROM router_events WHERE id='router.retrying'";
        let retrying_count =
            |conn: &Connection| -> i64 { conn.query_row(retrying, [], |row| row.get(0)).unwrap() };
        bus.finish(&conn, 1, "a", StepOutcome::Fail("x".into()), 0)
            .unwrap();
        assert_eq!(retrying_count(&conn), 0, "16 s backoff is below the cap");
        bus.finish(&conn, 1, "a", StepOutcome::Fail("x".into()), 0)
            .unwrap();
        assert_eq!(retrying_count(&conn), 1);
        let event = load(&conn, 2).unwrap();
        assert_eq!(event.correlation, Some(uid));
        assert_eq!(event.payload["attempts"], json!(6));
        assert_eq!(event.payload["plugin"], json!("a"));
        bus.finish(&conn, 1, "a", StepOutcome::Fail("x".into()), 0)
            .unwrap();
        assert_eq!(
            retrying_count(&conn),
            1,
            "only the first time the cap is reached"
        );
    }

    #[tokio::test]
    async fn unfinished_work_replays_after_restart_and_removed_plugins_are_skipped() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("router.db");
        let path = path.to_str().unwrap();
        let log = Log::default();
        let plugins = || {
            vec![
                plugin("a", &[], vec![record("a.exec", &log)]),
                plugin("b", &["a"], vec![record("b.exec", &log)]),
            ]
        };
        {
            let mut before = plugins();
            before.push(plugin("gone", &[], Vec::new()));
            let bus = idle_bus(db::open(path).unwrap(), boxes(before));
            let conn = bus.db.lock().unwrap();
            bus.emit_in(&conn, "test", EVENT, 1, json!({}), None)
                .unwrap();
            // "a" was mid-run when the process died.
            conn.execute(
                "UPDATE router_deliveries SET state='running' WHERE plugin='a'",
                [],
            )
            .unwrap();
        }
        let db = db::open(path).unwrap();
        let (_bus, _router) = start(db.clone(), events(), boxes(plugins())).unwrap();
        settle(&db).await;
        assert_eq!(logged(&log), ["a.exec", "b.exec"]);
        let states: Vec<_> = states(&db)
            .into_iter()
            .map(|(plugin, state, _)| (plugin, state))
            .collect();
        assert_eq!(
            states,
            [
                ("a".into(), "completed".into()),
                ("b".into(), "completed".into()),
                ("gone".into(), "skipped".into())
            ]
        );
    }

    #[test]
    fn registration_rejects_invalid_plugin_graphs() {
        let rejected = |plugins: Vec<TestPlugin>| Registry::new(events(), boxes(plugins)).is_err();
        assert!(rejected(vec![
            plugin("a", &["b"], Vec::new()),
            plugin("b", &["a"], Vec::new())
        ]));
        assert!(rejected(vec![plugin("a", &["a"], Vec::new())]));
        assert!(rejected(vec![plugin("a", &["missing"], Vec::new())]));
        let elsewhere = TestPlugin {
            event: OTHER,
            ..plugin("x", &[], Vec::new())
        };
        assert!(rejected(vec![plugin("a", &["x"], Vec::new()), elsewhere]));
        let undeclared = TestPlugin {
            event: "test.missing",
            ..plugin("a", &[], Vec::new())
        };
        assert!(rejected(vec![undeclared]));
        assert!(rejected(vec![
            plugin("a", &[], Vec::new()),
            plugin("a", &[], Vec::new())
        ]));
        assert!(!rejected(vec![
            plugin("a", &[], Vec::new()),
            plugin("b", &["a"], Vec::new()),
            plugin("c", &["a", "b"], Vec::new())
        ]));
    }

    #[test]
    fn emit_in_joins_the_callers_transaction() {
        let db = db::open(":memory:").unwrap();
        let bus = idle_bus(db.clone(), boxes(vec![plugin("a", &[], Vec::new())]));
        {
            let conn = db.lock().unwrap();
            let tx = conn.unchecked_transaction().unwrap();
            bus.emit_in(&tx, "test", EVENT, 1, json!({}), None).unwrap();
            tx.rollback().unwrap();
        }
        assert_eq!(count(&db, "SELECT COUNT(*) FROM router_events"), 0);
        assert_eq!(count(&db, "SELECT COUNT(*) FROM router_deliveries"), 0);
    }

    #[test]
    fn prune_removes_only_finished_events_older_than_the_cutoff() {
        let db = db::open(":memory:").unwrap();
        let bus = idle_bus(db.clone(), boxes(vec![plugin("a", &[], Vec::new())]));
        let conn = db.lock().unwrap();
        bus.emit_in(&conn, "test", EVENT, 1, json!({}), None)
            .unwrap();
        bus.emit_in(&conn, "test", EVENT, 1, json!({}), None)
            .unwrap();
        bus.finish(&conn, 1, "a", StepOutcome::Continue, 0).unwrap();
        assert_eq!(prune(&conn, 0, None).unwrap(), 0, "newer than the cutoff");
        assert_eq!(prune(&conn, i64::MAX, None).unwrap(), 1);
        let seq: i64 = conn
            .query_row("SELECT seq FROM router_events", [], |row| row.get(0))
            .unwrap();
        assert_eq!(seq, 2, "the event still being delivered is kept");
    }
}
