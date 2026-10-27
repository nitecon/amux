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
    ops::RangeInclusive,
    sync::Arc,
    time::Duration,
};
use tokio::{sync::Notify, task::spawn_blocking};
use uuid::Uuid;

/// Emitted once when a delivery first reaches the retry cap.
pub const RETRYING: &str = "router.retrying";
const FIRST_RETRY_MS: i64 = 1_000;
const MAX_RETRY_MS: i64 = 30_000;
const TERMINAL: &str = "('completed','stopped','skipped')";

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
        );
        CREATE TABLE IF NOT EXISTS router_deliveries (
            event_seq       INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,
            plugin          TEXT NOT NULL,
            state           TEXT NOT NULL CHECK(state IN
                ('pending','running','retrying','completed','stopped','skipped')),
            attempts        INTEGER NOT NULL DEFAULT 0,
            next_attempt_at INTEGER NOT NULL,
            last_error      TEXT,
            updated_at      INTEGER NOT NULL,
            PRIMARY KEY(event_seq, plugin)
        );
        CREATE INDEX IF NOT EXISTS idx_router_deliveries_open
            ON router_deliveries(state, next_attempt_at);",
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
    /// Retry the whole run from its first step.
    Fail(String),
}

#[async_trait]
pub trait Step: Send + Sync {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome;
}

pub trait Plugin: Send + Sync {
    fn id(&self) -> &'static str;
    fn subscriptions(&self) -> Vec<Subscription>;
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
}

struct Registered {
    phases: [Vec<Box<dyn Step>>; 3],
    /// event id -> depends_on
    depends_on: HashMap<&'static str, Vec<&'static str>>,
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
                if !registry
                    .events
                    .keys()
                    .any(|(event, _)| event == subscription.event)
                {
                    bail!(
                        "plugin {id} subscribes to undeclared event {}",
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
            if registry
                .plugins
                .insert(id, Registered { phases, depends_on })
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
            let subscribed: HashSet<_> = subscribers.iter().map(|(plugin, _)| *plugin).collect();
            for plugin in &subscribed {
                for dependency in &self.plugins[plugin].depends_on[event] {
                    if !subscribed.contains(dependency) {
                        bail!("plugin {plugin} depends on {dependency}, which does not subscribe to {event}");
                    }
                }
            }
            let mut done = HashSet::new();
            for plugin in &subscribed {
                self.acyclic(event, plugin, &mut Vec::new(), &mut done)?;
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

    /// Whether authenticated remote clients may emit this event.
    pub fn accepts_remote(&self, id: &str, version: u32) -> bool {
        self.registry
            .events
            .get(&(id.to_owned(), version))
            .is_some_and(|definition| definition.remote_emit)
    }

    /// Mark runnable deliveries running.
    fn claim(&self, now: i64) -> Result<Claimed> {
        let conn = self.db.lock().unwrap();
        let tx = conn.unchecked_transaction()?;
        let open = {
            let mut statement = tx.prepare(
                "SELECT d.event_seq, d.plugin, e.id, d.next_attempt_at
                 FROM router_deliveries d JOIN router_events e ON e.seq = d.event_seq
                 WHERE d.state IN ('pending','retrying') ORDER BY d.event_seq, d.plugin",
            )?;
            let rows = statement.query_map([], |row| {
                Ok((
                    row.get::<_, i64>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, i64>(3)?,
                ))
            })?;
            rows.collect::<rusqlite::Result<Vec<_>>>()?
        };
        let (mut runs, mut next, mut skipped) = (Vec::new(), None::<i64>, false);
        for (seq, plugin, event, due) in open {
            let Some((plugin, registered)) = self.registry.plugins.get_key_value(plugin.as_str())
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
                    None | Some("completed") => {}
                    Some("stopped" | "skipped") => blocked = true,
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
    };
    Ok((bus.clone(), tokio::spawn(route(bus))))
}

/// Runs interrupted by a restart run again; deliveries for plugins that are no
/// longer registered can never run, so they are skipped.
fn recover(conn: &Connection, registry: &Registry) -> Result<()> {
    let now = db::now_ms();
    conn.execute(
        "UPDATE router_deliveries SET state='pending', updated_at=?1 WHERE state='running'",
        params![now],
    )?;
    let plugins = conn
        .prepare(&format!(
            "SELECT DISTINCT plugin FROM router_deliveries WHERE state NOT IN {TERMINAL}"
        ))?
        .query_map([], |row| row.get::<_, String>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for plugin in plugins
        .iter()
        .filter(|plugin| !registry.plugins.contains_key(plugin.as_str()))
    {
        conn.execute(
            &format!(
                "UPDATE router_deliveries SET state='skipped', last_error='plugin is not registered',
                 updated_at=?2 WHERE plugin=?1 AND state NOT IN {TERMINAL}"
            ),
            params![plugin, now],
        )?;
    }
    Ok(())
}

async fn route(bus: Bus) {
    loop {
        let claimer = bus.clone();
        let next = match spawn_blocking(move || claimer.claim(db::now_ms())).await {
            Ok(Ok(Claimed {
                runs,
                next,
                skipped,
            })) => {
                for (event, plugin) in runs {
                    tokio::spawn(execute(bus.clone(), event, plugin));
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
            _ = bus.wake.notified() => {}
            _ = tokio::time::sleep(wait) => {}
        }
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
    let outcome = tokio::spawn(async move {
        for phase in &registry.plugins[plugin].phases {
            for step in phase {
                match step.run(&event, &ctx).await {
                    StepOutcome::Continue => {}
                    outcome => return outcome,
                }
            }
        }
        StepOutcome::Continue
    })
    .await
    .unwrap_or_else(|error| StepOutcome::Fail(format!("plugin panicked: {error}")));
    let finisher = bus.clone();
    let finished = spawn_blocking(move || {
        let conn = finisher.db.lock().unwrap();
        let tx = conn.unchecked_transaction()?;
        finisher.finish(&tx, seq, plugin, outcome, db::now_ms())?;
        tx.commit()?;
        anyhow::Ok(())
    })
    .await;
    match finished {
        Ok(Ok(())) => {}
        Ok(Err(error)) => {
            tracing::warn!("Event router could not record {plugin} for event {seq}: {error}")
        }
        Err(error) => tracing::warn!("Event router record task failed: {error}"),
    }
    bus.wake();
}

/// Remove finished events older than `cutoff_ms`.
pub fn prune(conn: &Connection, cutoff_ms: i64) -> Result<usize> {
    Ok(conn.execute(
        &format!(
            "DELETE FROM router_events WHERE emitted_at < ?1 AND NOT EXISTS (
             SELECT 1 FROM router_deliveries d WHERE d.event_seq = router_events.seq
             AND d.state NOT IN {TERMINAL})"
        ),
        params![cutoff_ms],
    )?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        atomic::{AtomicBool, Ordering},
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
    }

    fn plugin(id: &'static str, depends_on: &[&'static str], exec: Vec<Record>) -> TestPlugin {
        TestPlugin {
            id,
            event: EVENT,
            depends_on: depends_on.to_vec(),
            pre: Vec::new(),
            exec,
            post: Vec::new(),
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
                versions: 1..=1,
                depends_on: self.depends_on.clone(),
            }]
        }
        fn pre_exec(&self) -> Vec<Box<dyn Step>> {
            boxed(&self.pre)
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
        assert_eq!(prune(&conn, 0).unwrap(), 0, "newer than the cutoff");
        assert_eq!(prune(&conn, i64::MAX).unwrap(), 1);
        let seq: i64 = conn
            .query_row("SELECT seq FROM router_events", [], |row| row.get(0))
            .unwrap();
        assert_eq!(seq, 2, "the event still being delivered is kept");
    }
}
