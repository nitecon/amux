//! Presence: derives agent liveness and idle/busy state from what clients
//! report. It consumes `client.heartbeat`, `terminal.activity` and
//! `clock.tick`, and emits `agent.online`, `agent.idle`, `agent.busy` and
//! `agent.gone` on transitions. Router sequence fences reports; evaluation
//! time advances monotonically from the events' `emitted_at` values.
use crate::{
    db,
    plugins::{clock, remote},
    router::{Event, EventDefinition, Plugin, Step, StepContext, StepOutcome, Subscription},
    task_dispatch::Target,
    task_events::AgentOrigin,
    task_targets,
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection, OptionalExtension};
use serde_json::{json, Value};

pub const PLUGIN: &str = "presence";
pub const ONLINE: &str = "agent.online";
pub const IDLE: &str = "agent.idle";
pub const BUSY: &str = "agent.busy";
pub const GONE: &str = "agent.gone";
/// An agent is idle after this long without terminal output and no draft.
pub const IDLE_AFTER_MS: i64 = 30_000;
/// An agent whose client stopped sending heartbeats is gone.
pub const GONE_AFTER_MS: i64 = 90_000;

pub fn events() -> Vec<EventDefinition> {
    [ONLINE, IDLE, BUSY, GONE]
        .into_iter()
        .map(|id| EventDefinition {
            id,
            version: 1,
            remote_emit: false,
            validate: agent_payload,
        })
        .collect()
}

/// `{"agent": {...}}` as reported in the client heartbeat, plus its `source`.
fn agent_payload(payload: &Value) -> std::result::Result<(), String> {
    if payload["agent"]["session_id"].is_string() && payload["agent"]["project"].is_string() {
        Ok(())
    } else {
        Err("payload needs an agent with a session_id and project".into())
    }
}

const DIRECTORY_SCHEMA: &str = "CREATE TABLE IF NOT EXISTS task_target_presence (
    project_ident TEXT NOT NULL, instance_id TEXT NOT NULL, session_id TEXT NOT NULL,
    origin_json TEXT NOT NULL, generation TEXT NOT NULL,
    seen_at INTEGER NOT NULL DEFAULT 0, canonical_remote TEXT NOT NULL DEFAULT '',
    display_name TEXT, hostname TEXT, advertised INTEGER NOT NULL DEFAULT 1,
    source TEXT, agent_json TEXT, report_seen_at INTEGER, output_at INTEGER,
    draft INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'unknown' CHECK(state IN ('unknown','busy','idle')),
    identity_seq INTEGER NOT NULL DEFAULT 0, activity_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(project_ident,instance_id,session_id)
);";

/// One identity directory serves REST advertisements and event reports. The
/// separate leases preserve both APIs; observation alone never grants routing.
pub fn initialize_directory(conn: &Connection) -> Result<()> {
    conn.execute_batch(DIRECTORY_SCHEMA)?;
    let columns: Vec<String> = conn
        .prepare("SELECT name FROM pragma_table_info('task_target_presence')")?
        .query_map([], |row| row.get(0))?
        .collect::<rusqlite::Result<_>>()?;
    if !columns.iter().any(|column| column == "advertised") {
        for column in ["display_name", "hostname"] {
            if !columns.iter().any(|existing| existing == column) {
                conn.execute_batch(&format!(
                    "ALTER TABLE task_target_presence ADD COLUMN {column} TEXT"
                ))?;
            }
        }
        // Event-only v1 reports may observe unregistered projects. REST writes
        // still validate project/repository binding before advertising them.
        let tx = conn.unchecked_transaction()?;
        tx.execute_batch(
            "ALTER TABLE task_target_presence RENAME TO task_target_presence_legacy;",
        )?;
        tx.execute_batch(DIRECTORY_SCHEMA)?;
        tx.execute_batch(
            "INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,display_name,hostname)
             SELECT project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,display_name,hostname FROM task_target_presence_legacy;
             DROP TABLE task_target_presence_legacy;",
        )?;
        tx.commit()?;
    }
    conn.execute_batch(
        "CREATE INDEX IF NOT EXISTS presence_source ON task_target_presence(source);
         CREATE INDEX IF NOT EXISTS presence_session ON task_target_presence(session_id,instance_id);
         CREATE TRIGGER IF NOT EXISTS presence_project_deleted AFTER DELETE ON projects BEGIN
            DELETE FROM task_target_presence WHERE project_ident=OLD.ident;
         END;",
    )?;
    Ok(())
}

pub fn initialize(conn: &Connection) -> Result<()> {
    initialize_directory(conn)?;
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS presence_sources (
            source TEXT PRIMARY KEY,
            heartbeat_seq INTEGER NOT NULL
        );
        CREATE TABLE IF NOT EXISTS presence_clock (
            id INTEGER PRIMARY KEY CHECK(id=1),
            observed_at INTEGER NOT NULL
        );
        INSERT OR IGNORE INTO presence_clock(id,observed_at) VALUES(1,0);",
    )?;
    let legacy: bool = conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='presence_agents')",
        [],
        |row| row.get(0),
    )?;
    if legacy {
        migrate_agents(conn)?;
    }
    Ok(())
}

pub fn target_connected(conn: &Connection, project: &str, target: &Target) -> Result<bool> {
    Ok(conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM task_target_presence s JOIN projects p ON p.ident=s.project_ident
         AND (p.canonical_remote=s.canonical_remote OR (p.kind='adhoc' AND p.canonical_remote IS NULL AND s.canonical_remote=''))
         WHERE s.advertised=1 AND s.project_ident=?1 AND s.instance_id=?2 AND s.session_id=?3
         AND s.origin_json=?4 AND s.generation=?5 AND s.seen_at>=?6)",
        params![project,target.origin.instance_id,target.origin.session_id,serde_json::to_string(&target.origin)?,target.generation,db::now_ms()-task_targets::TTL_MS],
        |row| row.get(0),
    )?)
}

pub fn targets(conn: &Connection, project: &str) -> Result<Vec<Value>> {
    let rows = conn.prepare(
        "SELECT s.origin_json,s.generation,s.seen_at,s.display_name,s.hostname
         FROM task_target_presence s JOIN projects p ON p.ident=s.project_ident
         AND (p.canonical_remote=s.canonical_remote OR (p.kind='adhoc' AND p.canonical_remote IS NULL AND s.canonical_remote=''))
         WHERE s.advertised=1 AND s.project_ident=?1 AND s.seen_at>=?2 ORDER BY s.instance_id,s.session_id",
    )?.query_map(params![project,db::now_ms()-task_targets::TTL_MS], |row| {
        Ok((row.get::<_,String>(0)?,row.get::<_,String>(1)?,row.get::<_,i64>(2)?,row.get::<_,Option<String>>(3)?,row.get::<_,Option<String>>(4)?))
    })?.collect::<rusqlite::Result<Vec<_>>>()?;
    rows.into_iter().map(|(origin,generation,seen,display_name,hostname)| {
        Ok(json!({"target":Target {origin:serde_json::from_str(&origin)?,generation},"seen_at":seen,"expires_at":seen+task_targets::TTL_MS,"display_name":display_name,"hostname":hostname}))
    }).collect()
}

pub fn advertise(
    conn: &Connection,
    project: &str,
    target: &Target,
    seen: i64,
    remote: Option<&str>,
    name: Option<&str>,
    host: Option<&str>,
) -> Result<()> {
    let origin = serde_json::to_string(&target.origin)?;
    // Advertisement identity and lease do not authorize changes to the
    // independently authenticated event report or its source ownership.
    conn.execute(
        "INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,display_name,hostname)
         VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9) ON CONFLICT(project_ident,instance_id,session_id)
         DO UPDATE SET origin_json=excluded.origin_json,generation=excluded.generation,seen_at=excluded.seen_at,
         canonical_remote=excluded.canonical_remote,display_name=excluded.display_name,hostname=excluded.hostname,advertised=1",
        params![project,target.origin.instance_id,target.origin.session_id,origin,target.generation,seen,remote.unwrap_or(""),name,host],
    )?;
    Ok(())
}

pub fn registered_host(conn: &Connection, project: &str, origin: &Value) -> Result<Option<String>> {
    let Some(session) = origin["session_id"].as_str() else {
        return Ok(None);
    };
    // Only explicit advertisements write this column. Historical display
    // metadata remains useful after routing authority or event liveness ends.
    Ok(conn
        .query_row(
            "SELECT hostname FROM task_target_presence WHERE session_id=?2
         AND hostname IS NOT NULL AND trim(hostname)!=''
         ORDER BY (instance_id=?3) DESC,(project_ident=?1) DESC,seen_at DESC LIMIT 1",
            params![
                project,
                session,
                origin["instance_id"].as_str().unwrap_or_default()
            ],
            |row| row.get(0),
        )
        .optional()?)
}

fn clear_report(conn: &Connection, project: &str, instance: &str, session: &str) -> Result<()> {
    conn.execute(
        "UPDATE task_target_presence SET source=NULL,agent_json=NULL,report_seen_at=NULL,output_at=NULL,
         draft=0,state='unknown',identity_seq=0,activity_seq=0
         WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3",
        params![project,instance,session],
    )?;
    Ok(())
}

/// Authoritative Presence read contract for controllers. Consumers use this
/// snapshot instead of querying Presence's tables or copying transition state.
pub struct AgentPresence {
    pub session_id: String,
    pub agent: Value,
    pub output_at: Option<i64>,
    pub draft: bool,
    pub state: String,
}

impl AgentPresence {
    pub fn idle(&self, now: i64) -> bool {
        self.state == "idle"
            && !self.draft
            && self.output_at.is_some_and(|at| now - at >= IDLE_AFTER_MS)
    }
}

/// Live reports, oldest output first. Clock consumers depend on Presence for
/// the same tick so its state evaluation has committed before they read.
pub fn agents(conn: &Connection, now: i64) -> Result<Vec<AgentPresence>> {
    let rows = conn
        .prepare(
            "SELECT session_id,agent_json,output_at,draft,state FROM task_target_presence
             WHERE report_seen_at IS NOT NULL AND ?1-report_seen_at < ?2 ORDER BY output_at,session_id,instance_id,project_ident",
        )?
        .query_map(params![now, GONE_AFTER_MS], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, Option<i64>>(2)?,
                row.get::<_, bool>(3)?,
                row.get::<_, String>(4)?,
            ))
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    rows.into_iter()
        .filter_map(|(session_id, agent, output_at, draft, state)| {
            let result = (|| -> Result<Option<AgentPresence>> {
                let agent: Value = serde_json::from_str(&agent)?;
                if !valid_binding(conn, &agent)? {
                    return Ok(None);
                }
                Ok(Some(AgentPresence {
                    session_id,
                    agent,
                    output_at,
                    draft,
                    state,
                }))
            })();
            result.transpose()
        })
        .collect()
}

/// Whether this heartbeat is the current, unexpired complete source report.
/// Controllers observing heartbeats after Presence must ignore reordered runs.
pub fn heartbeat_is_current(conn: &Connection, event: &Event) -> Result<bool> {
    Ok(conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM presence_sources s, presence_clock c
         WHERE s.source=?1 AND s.heartbeat_seq=?2 AND c.id=1 AND c.observed_at-?3<?4)",
        params![event.source, event.seq, event.emitted_at, GONE_AFTER_MS],
        |row| row.get(0),
    )?)
}

/// Last reported output, used to hold an undeliverable assignment until the
/// refusing session produces new output.
pub fn last_output(conn: &Connection, session: &str) -> Result<Option<i64>> {
    Ok(conn
        .query_row(
            "SELECT MAX(output_at) FROM task_target_presence WHERE session_id=?1 AND report_seen_at IS NOT NULL",
            params![session],
            |row| row.get::<_, Option<i64>>(0),
        )
        .optional()?
        .flatten())
}

/// Live agents across the gateway: distinct client machines, their OSes and
/// how many agents are busy. Gone agents are already deleted.
pub fn summary(conn: &Connection) -> Result<Value> {
    let agents = agents(conn, db::now_ms())?;
    let machines: std::collections::HashSet<_> = agents
        .iter()
        .filter_map(|agent| agent.agent["source"].as_str())
        .collect();
    let os: std::collections::BTreeSet<_> = agents
        .iter()
        .filter_map(|agent| agent.agent["os"].as_str())
        .collect();
    let active = agents.iter().filter(|agent| agent.state == "busy").count();
    Ok(json!({"machines": machines.len(), "os": os, "agents": agents.len(), "active": active}))
}

/// `GET /v1/presence/summary`.
pub async fn summary_route(
    axum::extract::State(state): axum::extract::State<crate::AppState>,
) -> std::result::Result<axum::Json<Value>, crate::routes::AppError> {
    let conn = state.db.lock().unwrap();
    Ok(axum::Json(summary(&conn)?))
}

pub struct Presence;

impl Plugin for Presence {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        [remote::HEARTBEAT, remote::ACTIVITY, clock::TICK]
            .into_iter()
            .map(|event| Subscription {
                event,
                versions: 1..=1,
                depends_on: Vec::new(),
            })
            .collect()
    }
    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Track)]
    }
}

struct Track;

#[async_trait]
impl Step for Track {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let result = (|| -> Result<()> {
            let conn = ctx.db().lock().unwrap();
            let tx = conn.unchecked_transaction()?;
            match event.id.as_str() {
                remote::HEARTBEAT => heartbeat(&tx, ctx, event)?,
                remote::ACTIVITY => activity(&tx, ctx, event)?,
                _ => tick(&tx, ctx, event.emitted_at)?,
            }
            tx.commit()?;
            Ok(())
        })();
        ctx.wake();
        match result {
            Ok(()) => StepOutcome::Continue,
            Err(error) => StepOutcome::Fail(error.to_string()),
        }
    }
}

type AgentKey = (String, String, String);

fn agent_key(agent: &Value) -> AgentKey {
    (
        agent["project"].as_str().unwrap_or_default().to_owned(),
        agent["instance_id"].as_str().unwrap_or_default().to_owned(),
        agent["session_id"].as_str().unwrap_or_default().to_owned(),
    )
}

fn origin(agent: &Value) -> AgentOrigin {
    AgentOrigin {
        session_id: agent["session_id"].as_str().unwrap_or_default().to_owned(),
        instance_id: agent["instance_id"].as_str().unwrap_or_default().to_owned(),
        provider: agent["provider"].as_str().unwrap_or_default().to_owned(),
        os: agent["os"].as_str().unwrap_or_default().to_owned(),
    }
}

/// Legacy event-only v1 reports have no repository marker. New native reports
/// carry one, and invalid individual bindings are rejected without retrying a
/// permanently invalid report or blocking its valid siblings.
fn valid_binding(conn: &Connection, agent: &Value) -> Result<bool> {
    let Some(remote) = agent.get("canonical_remote") else {
        return Ok(true);
    };
    if !remote.is_null() && !remote.is_string() {
        return Ok(false);
    }
    match task_targets::require_binding(
        conn,
        agent["project"].as_str().unwrap_or_default(),
        remote.as_str(),
    ) {
        Ok(_) => Ok(true),
        Err(error) if error.0.is_client_error() => Ok(false),
        Err(error) => Err(anyhow::anyhow!(error.1)),
    }
}

/// Store an event observation on its directory row. Only an explicit REST
/// advertisement may set its identity and lease. Reports update only the event
/// snapshot and cannot create, renew or replace a manually routable target.
fn store_report(
    conn: &Connection,
    source: &str,
    agent: &Value,
    seen: i64,
    seq: i64,
) -> Result<Option<bool>> {
    if !valid_binding(conn, agent)? {
        return Ok(None);
    }
    let key = agent_key(agent);
    let foreign: bool = conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM task_target_presence WHERE session_id=?1
         AND report_seen_at IS NOT NULL AND source!=?2)",
        params![key.2, source],
        |row| row.get(0),
    )?;
    if foreign {
        return Ok(None);
    }
    let raw_origin = serde_json::to_string(&origin(agent))?;
    let generation = agent["generation"].as_str().unwrap_or_default();
    let previous: Option<String> = conn
        .query_row(
            "SELECT agent_json FROM task_target_presence
         WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3",
            params![key.0, key.1, key.2],
            |row| row.get::<_, Option<String>>(0),
        )
        .optional()?
        .flatten();
    let changed = previous
        .as_ref()
        .and_then(|before| serde_json::from_str::<Value>(before).ok())
        .as_ref()
        != Some(agent);
    // A source reports one current execution per native conversation. Clear
    // its prior location when the same conversation moves project/instance.
    conn.execute(
        "UPDATE task_target_presence SET source=NULL,agent_json=NULL,report_seen_at=NULL,
         output_at=NULL,draft=0,state='unknown',identity_seq=0,activity_seq=0
         WHERE session_id=?3 AND source=?4 AND NOT(project_ident=?1 AND instance_id=?2)",
        params![key.0, key.1, key.2, source],
    )?;
    conn.execute("DELETE FROM task_target_presence WHERE session_id=?3 AND advertised=0 AND report_seen_at IS NULL
                  AND seen_at=0 AND hostname IS NULL AND display_name IS NULL
                  AND NOT(project_ident=?1 AND instance_id=?2)", params![key.0,key.1,key.2])?;
    conn.execute(
        "INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,advertised,source,agent_json,report_seen_at,identity_seq)
         VALUES(?1,?2,?3,?4,?5,0,?6,?7,?8,?9)
         ON CONFLICT(project_ident,instance_id,session_id) DO UPDATE SET source=?6,agent_json=?7,
         report_seen_at=MAX(COALESCE(report_seen_at,?8),?8)",
        params![key.0,key.1,key.2,raw_origin,generation,source,agent.to_string(),seen,seq],
    )?;
    if changed {
        conn.execute(
            "UPDATE task_target_presence SET output_at=NULL,draft=0,state='unknown',identity_seq=?4,activity_seq=0
             WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3",
            params![key.0,key.1,key.2,seq],
        )?;
    }
    Ok(Some(changed))
}

fn migrate_agents(conn: &Connection) -> Result<()> {
    let tx = conn.unchecked_transaction()?;
    for column in ["identity_seq", "activity_seq"] {
        let present: bool = tx.query_row(
            "SELECT EXISTS(SELECT 1 FROM pragma_table_info('presence_agents') WHERE name=?1)",
            params![column],
            |row| row.get(0),
        )?;
        if !present {
            tx.execute_batch(&format!(
                "ALTER TABLE presence_agents ADD COLUMN {column} INTEGER NOT NULL DEFAULT 0"
            ))?;
        }
    }
    let rows = tx.prepare("SELECT source,agent_json,seen_at,output_at,draft,state,identity_seq,activity_seq FROM presence_agents")?
        .query_map([], |row| Ok((row.get::<_,String>(0)?,row.get::<_,String>(1)?,row.get::<_,i64>(2)?,row.get::<_,Option<i64>>(3)?,row.get::<_,bool>(4)?,row.get::<_,String>(5)?,row.get::<_,i64>(6)?,row.get::<_,i64>(7)?)))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for (source, agent, seen, output, draft, state, identity_seq, activity_seq) in rows {
        let agent: Value = serde_json::from_str(&agent)?;
        if store_report(&tx, &source, &agent, seen, identity_seq)?.is_some() {
            let key = agent_key(&agent);
            tx.execute("UPDATE task_target_presence SET output_at=?4,draft=?5,state=?6,activity_seq=?7 WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3", params![key.0,key.1,key.2,output,draft,state,activity_seq])?;
        }
    }
    tx.execute_batch("DROP TABLE presence_agents;")?;
    tx.commit()?;
    Ok(())
}

fn heartbeat(conn: &Connection, ctx: &StepContext, event: &Event) -> Result<()> {
    let now = event.emitted_at;
    let observed = observe_time(conn, now)?;
    // A heartbeat is the complete list for one source. Keep its cursor after
    // agents depart, otherwise replay could restore an omitted agent.
    let accepted = conn.execute(
        "INSERT INTO presence_sources(source,heartbeat_seq) VALUES(?1,?2)
         ON CONFLICT(source) DO UPDATE SET heartbeat_seq=?2 WHERE heartbeat_seq<?2",
        params![event.source, event.seq],
    )?;
    if accepted == 0 || observed - now >= GONE_AFTER_MS {
        return Ok(());
    }
    let mut listed = std::collections::HashSet::new();
    for agent in event.payload["agents"].as_array().into_iter().flatten() {
        let mut agent = agent.clone();
        agent["source"] = json!(event.source);
        let Some(changed) = store_report(conn, &event.source, &agent, now, event.seq)? else {
            continue;
        };
        let key = agent_key(&agent);
        if changed {
            ctx.emit_in(conn, ONLINE, 1, json!({ "agent": agent }))?;
        }
        evaluate(conn, ctx, &key, observed)?;
        listed.insert(key);
    }
    let departed: Vec<AgentKey> = conn
        .prepare("SELECT project_ident,instance_id,session_id FROM task_target_presence WHERE source=?1 AND report_seen_at IS NOT NULL")?
        .query_map(params![event.source], |row| Ok((row.get(0)?,row.get(1)?,row.get(2)?)))?
        .collect::<rusqlite::Result<Vec<_>>>()?
        .into_iter()
        .filter(|key| !listed.contains(key))
        .collect();
    for key in departed {
        gone(conn, ctx, &key)?;
    }
    Ok(())
}

fn activity(conn: &Connection, ctx: &StepContext, event: &Event) -> Result<()> {
    let observed = observe_time(conn, event.emitted_at)?;
    let payload = &event.payload;
    let session = payload["session_id"].as_str().unwrap_or_default();
    let instance = payload["instance_id"].as_str().unwrap_or_default();
    let projects = conn
        .prepare(
            "SELECT project_ident FROM task_target_presence WHERE session_id=?1 AND source=?2
         AND instance_id=?3 AND report_seen_at IS NOT NULL AND identity_seq<?4 AND activity_seq<?4",
        )?
        .query_map(params![session, event.source, instance, event.seq], |row| {
            row.get::<_, String>(0)
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for project in projects {
        conn.execute(
            "UPDATE task_target_presence SET output_at=MAX(COALESCE(output_at,?4),COALESCE(?4,output_at)),
             draft=?5,activity_seq=?6 WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3",
            params![project,instance,session,payload["output_at"].as_i64(),payload["draft"].as_bool()==Some(true),event.seq],
        )?;
        evaluate(
            conn,
            ctx,
            &(project, instance.to_owned(), session.to_owned()),
            observed,
        )?;
    }
    Ok(())
}

fn tick(conn: &Connection, ctx: &StepContext, now: i64) -> Result<()> {
    let now = observe_time(conn, now)?;
    let sessions: Vec<(AgentKey, i64)> = conn
        .prepare("SELECT project_ident,instance_id,session_id,report_seen_at FROM task_target_presence WHERE report_seen_at IS NOT NULL")?
        .query_map([], |row| Ok(((row.get(0)?,row.get(1)?,row.get(2)?), row.get(3)?)))?
        .collect::<rusqlite::Result<_>>()?;
    for (session, seen_at) in sessions {
        if now - seen_at >= GONE_AFTER_MS {
            gone(conn, ctx, &session)?;
        } else {
            evaluate(conn, ctx, &session, now)?;
        }
    }
    Ok(())
}

/// Reports can finish out of router sequence. Liveness and derived state use
/// the newest server time observed by Presence, including after deletions.
fn observe_time(conn: &Connection, now: i64) -> Result<i64> {
    Ok(conn.query_row(
        "UPDATE presence_clock SET observed_at=MAX(observed_at,?1) WHERE id=1 RETURNING observed_at",
        params![now],
        |row| row.get(0),
    )?)
}

/// Emit `agent.idle` / `agent.busy` when the derived state changes.
fn evaluate(conn: &Connection, ctx: &StepContext, key: &AgentKey, now: i64) -> Result<()> {
    let (agent, output_at, draft, state): (String, Option<i64>, bool, String) = conn.query_row(
        "SELECT agent_json, output_at, draft, state FROM task_target_presence WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3",
        params![key.0,key.1,key.2],
        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
    )?;
    // Without any activity report the state stays unknown.
    let Some(output_at) = output_at else {
        return Ok(());
    };
    let next = if !draft && now - output_at >= IDLE_AFTER_MS {
        "idle"
    } else {
        "busy"
    };
    if next == state {
        return Ok(());
    }
    conn.execute(
        "UPDATE task_target_presence SET state=?4 WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3",
        params![key.0,key.1,key.2,next],
    )?;
    let id = if next == "idle" { IDLE } else { BUSY };
    let agent: Value = serde_json::from_str(&agent)?;
    ctx.emit_in(conn, id, 1, json!({ "agent": agent }))?;
    Ok(())
}

fn gone(conn: &Connection, ctx: &StepContext, key: &AgentKey) -> Result<()> {
    let agent: String = conn.query_row(
        "SELECT agent_json FROM task_target_presence WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3",
        params![key.0,key.1,key.2],
        |row| row.get(0),
    )?;
    let agent: Value = serde_json::from_str(&agent)?;
    clear_report(conn, &key.0, &key.1, &key.2)?;
    conn.execute("DELETE FROM task_target_presence WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3
                  AND advertised=0 AND seen_at=0 AND hostname IS NULL AND display_name IS NULL", params![key.0,key.1,key.2])?;
    ctx.emit_in(conn, GONE, 1, json!({ "agent": agent }))?;
    Ok(())
}

#[cfg(test)]
pub(crate) fn seed_report(
    conn: &Connection,
    source: &str,
    mut agent: Value,
    seen: i64,
    output: Option<i64>,
    draft: bool,
    state: &str,
) {
    agent["source"] = json!(source);
    assert!(store_report(conn, source, &agent, seen, 0)
        .unwrap()
        .is_some());
    let key = agent_key(&agent);
    conn.execute("UPDATE task_target_presence SET output_at=?4,draft=?5,state=?6 WHERE project_ident=?1 AND instance_id=?2 AND session_id=?3", params![key.0,key.1,key.2,output,draft,state]).unwrap();
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{db, plugins, router};
    use std::time::Duration;

    const SESSION: &str = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001";

    fn agent() -> Value {
        json!({"session_id": SESSION, "instance_id": "i", "provider": "claude", "os": "linux", "project": "p"})
    }

    fn report(id: &str, seq: i64, at: i64, payload: Value) -> Event {
        Event {
            seq,
            id: id.into(),
            version: 1,
            uid: uuid::Uuid::now_v7(),
            payload,
            source: "client:a".into(),
            emitted_at: at,
            correlation: None,
        }
    }

    fn frozen() -> (db::Db, router::Bus) {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, worker) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        worker.abort();
        (database, bus)
    }

    #[test]
    fn legacy_directory_and_event_state_merge_without_granting_observations_routing_authority() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        let now = db::now_ms();
        conn.execute_batch(
            "DROP TRIGGER presence_project_deleted;
             DROP TABLE task_target_presence;
             CREATE TABLE task_target_presence (
                project_ident TEXT NOT NULL REFERENCES projects(ident),instance_id TEXT NOT NULL,session_id TEXT NOT NULL,
                origin_json TEXT NOT NULL,generation TEXT NOT NULL,seen_at INTEGER NOT NULL,canonical_remote TEXT NOT NULL,
                PRIMARY KEY(project_ident,instance_id,session_id));
             CREATE TABLE presence_agents (session_id TEXT PRIMARY KEY,source TEXT NOT NULL,agent_json TEXT NOT NULL,
                seen_at INTEGER NOT NULL,output_at INTEGER,draft INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'unknown');
             INSERT INTO projects(ident,channel_name,room_id,kind,created_at) VALUES('p','discord','','adhoc',0);",
        ).unwrap();
        let mut advertised_agent = agent();
        advertised_agent["generation"] = json!("generation");
        advertised_agent["source"] = json!("client:a");
        let target = Target {
            origin: origin(&advertised_agent),
            generation: "generation".into(),
        };
        conn.execute("INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote) VALUES('p',?1,?2,?3,'generation',?4,'')", params![target.origin.instance_id,target.origin.session_id,serde_json::to_string(&target.origin).unwrap(),now]).unwrap();
        let observer = json!({"session_id":"observation","instance_id":"instance","provider":"codex","os":"linux","project":"unregistered","source":"client:b"});
        for (source, agent, state) in [
            ("client:a", advertised_agent, "busy"),
            ("client:b", observer, "idle"),
        ] {
            conn.execute("INSERT INTO presence_agents(session_id,source,agent_json,seen_at,output_at,state) VALUES(?1,?2,?3,?4,?5,?6)", params![agent["session_id"].as_str(),source,agent.to_string(),now,now-40_000,state]).unwrap();
        }
        initialize(&conn).unwrap();
        initialize(&conn).unwrap();
        assert!(target_connected(&conn, "p", &target).unwrap());
        assert_eq!(targets(&conn, "p").unwrap().len(), 1);
        assert_eq!(agents(&conn, now).unwrap().len(), 2);
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM task_target_presence", [], |row| row
                .get::<_, i64>(
                0
            ))
            .unwrap(),
            2
        );
        assert!(!conn
            .query_row(
                "SELECT advertised FROM task_target_presence WHERE session_id='observation'",
                [],
                |row| row.get::<_, bool>(0)
            )
            .unwrap());
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='presence_agents'",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            0
        );
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM pragma_foreign_key_list('task_target_presence')",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            0
        );
        conn.execute("DELETE FROM projects WHERE ident='p'", [])
            .unwrap();
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM task_target_presence WHERE project_ident='p'",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            0
        );
    }

    #[tokio::test]
    async fn event_and_advertisement_leases_share_identity_but_preserve_independent_contracts() {
        let (database, bus) = frozen();
        let conn = database.lock().unwrap();
        let ctx = context(&bus);
        let now = db::now_ms();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,kind,created_at) VALUES('p','discord','','adhoc',0)", []).unwrap();
        let mut report_agent = agent();
        report_agent["generation"] = json!("generation");
        let target = Target {
            origin: origin(&report_agent),
            generation: "generation".into(),
        };
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                1,
                now,
                json!({"agents":[report_agent.clone()]}),
            ),
        )
        .unwrap();
        assert_eq!(
            agents(&conn, now).unwrap().len(),
            1,
            "v1 needs no REST advertisement"
        );
        assert!(!target_connected(&conn, "p", &target).unwrap());
        advertise(&conn, "p", &target, now, None, Some("Codex"), Some("host")).unwrap();
        assert!(target_connected(&conn, "p", &target).unwrap());
        assert_eq!(agents(&conn, now).unwrap().len(), 1);
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM task_target_presence", [], |row| row
                .get::<_, i64>(
                0
            ))
            .unwrap(),
            1
        );
        tick(&conn, &ctx, now + GONE_AFTER_MS).unwrap();
        assert!(agents(&conn, now + GONE_AFTER_MS).unwrap().is_empty());
        assert!(
            target_connected(&conn, "p", &target).unwrap(),
            "event expiry cannot shorten advertised lease"
        );
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                2,
                now + GONE_AFTER_MS + 1,
                json!({"agents":[report_agent.clone()]}),
            ),
        )
        .unwrap();
        conn.execute(
            "UPDATE task_target_presence SET seen_at=?1",
            params![now - task_targets::TTL_MS - 1],
        )
        .unwrap();
        assert!(!target_connected(&conn, "p", &target).unwrap());
        assert_eq!(
            agents(&conn, now + GONE_AFTER_MS + 1).unwrap().len(),
            1,
            "expired ad cannot suppress v1 observation"
        );
        let replacement = Target {
            generation: "new-generation".into(),
            ..target
        };
        advertise(
            &conn,
            "p",
            &replacement,
            now + GONE_AFTER_MS + 2,
            None,
            None,
            None,
        )
        .unwrap();
        assert_eq!(
            agents(&conn, now + GONE_AFTER_MS + 2).unwrap()[0].agent["generation"],
            "generation",
            "advertisement does not replace the event execution"
        );
        for (seq, at) in [(3, now + GONE_AFTER_MS + 1), (4, now + GONE_AFTER_MS + 2)] {
            heartbeat(
                &conn,
                &ctx,
                &report(
                    remote::HEARTBEAT,
                    seq,
                    at,
                    json!({"agents":[report_agent.clone()]}),
                ),
            )
            .unwrap();
            assert_eq!(
                agents(&conn, now + GONE_AFTER_MS + 2).unwrap()[0].agent["generation"],
                "generation"
            );
            assert!(target_connected(&conn, "p", &replacement).unwrap());
        }
    }

    #[tokio::test]
    async fn delayed_rest_ad_cannot_erase_busy_state_or_release_event_source_ownership() {
        let (database, bus) = frozen();
        let conn = database.lock().unwrap();
        let ctx = context(&bus);
        let now = db::now_ms();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,kind,created_at) VALUES('p','discord','','adhoc',0)",[]).unwrap();
        let mut current = agent();
        current["generation"] = json!("current");
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                1,
                now,
                json!({"agents":[current.clone()]}),
            ),
        )
        .unwrap();
        activity(
            &conn,
            &ctx,
            &report(
                remote::ACTIVITY,
                2,
                now + 1,
                json!({"session_id":SESSION,"instance_id":"i","output_at":now,"draft":true}),
            ),
        )
        .unwrap();
        let old = Target {
            origin: origin(&current),
            generation: "old".into(),
        };
        advertise(
            &conn,
            "p",
            &old,
            now + 2,
            None,
            Some("Old execution"),
            Some("host"),
        )
        .unwrap();
        let current_report = agents(&conn, now + 2).unwrap().remove(0);
        assert_eq!(current_report.state, "busy");
        assert!(current_report.draft);
        assert_eq!(current_report.output_at, Some(now));
        assert_eq!(current_report.agent["generation"], "current");
        assert_eq!(current_report.agent["source"], "client:a");
        let mut foreign = report(
            remote::HEARTBEAT,
            3,
            now + 3,
            json!({"agents":[current.clone()]}),
        );
        foreign.source = "client:other".into();
        heartbeat(&conn, &ctx, &foreign).unwrap();
        assert_eq!(
            agents(&conn, now + 3).unwrap()[0].agent["source"],
            "client:a"
        );
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                4,
                now + 4,
                json!({"agents":[current.clone()]}),
            ),
        )
        .unwrap();
        assert!(
            target_connected(&conn, "p", &old).unwrap(),
            "event identity never silently replaces validated REST target"
        );
        assert!(!target_connected(
            &conn,
            "p",
            &Target {
                origin: origin(&current),
                generation: "current".into()
            }
        )
        .unwrap());
        assert_eq!(
            conn.query_row("SELECT seen_at FROM task_target_presence", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            now + 2,
            "event heartbeat never renews advertisement lease"
        );
        assert_eq!(agents(&conn, now + 4).unwrap()[0].state, "busy");
    }

    #[tokio::test]
    async fn registration_host_survives_routing_invalidation_rebinding_and_departure() {
        let (database, bus) = frozen();
        let conn = database.lock().unwrap();
        let ctx = context(&bus);
        let now = db::now_ms();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,kind,created_at) VALUES('p','discord','','adhoc',0)",[]).unwrap();
        let mut advertised_agent = agent();
        advertised_agent["generation"] = json!("old");
        advertised_agent["canonical_remote"] = Value::Null;
        let target = Target {
            origin: origin(&advertised_agent),
            generation: "old".into(),
        };
        advertise(
            &conn,
            "p",
            &target,
            now,
            None,
            Some("Original name"),
            Some("OriginalHost"),
        )
        .unwrap();
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                1,
                now + 1,
                json!({"agents":[advertised_agent.clone()]}),
            ),
        )
        .unwrap();
        let mut replacement = advertised_agent.clone();
        replacement["generation"] = json!("new");
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                2,
                now + 2,
                json!({"agents":[replacement]}),
            ),
        )
        .unwrap();
        assert!(
            target_connected(&conn, "p", &target).unwrap(),
            "event generation does not invalidate explicit advertisement"
        );
        conn.execute(
            "UPDATE task_target_presence SET seen_at=?1",
            params![now - task_targets::TTL_MS - 1],
        )
        .unwrap();
        assert!(!target_connected(&conn, "p", &target).unwrap());
        let owner = serde_json::to_value(&target.origin).unwrap();
        assert_eq!(
            registered_host(&conn, "p", &owner).unwrap().as_deref(),
            Some("OriginalHost")
        );
        let notification = crate::plugins::notify::emit(
            &conn,
            &ctx,
            json!({"target":SESSION,"project":"p","mode":"recipient","require_idle":false,
            "event":{"kind":"task_commented","origin":owner,"task":{"owner_origin":owner}}}),
        )
        .unwrap();
        let payload: String = conn
            .query_row(
                "SELECT payload FROM router_events WHERE uid=?1",
                [notification.to_string()],
                |row| row.get(0),
            )
            .unwrap();
        let payload: Value = serde_json::from_str(&payload).unwrap();
        assert_eq!(payload["event"]["sender_hostname"], "OriginalHost");
        assert_eq!(payload["event"]["owner_hostname"], "OriginalHost");
        conn.execute("UPDATE projects SET kind='repo',canonical_remote='github.com/new/project' WHERE ident='p'",[]).unwrap();
        assert!(agents(&conn, now + 2).unwrap().is_empty());
        assert_eq!(
            registered_host(&conn, "p", &owner).unwrap().as_deref(),
            Some("OriginalHost")
        );
        tick(&conn, &ctx, now + GONE_AFTER_MS + 3).unwrap();
        assert!(agents(&conn, now + GONE_AFTER_MS + 3).unwrap().is_empty());
        assert_eq!(
            registered_host(&conn, "p", &owner).unwrap().as_deref(),
            Some("OriginalHost")
        );
        let forged = json!({"session_id":"forged","instance_id":"instance","project":"unregistered","provider":"codex","os":"linux","hostname":"UntrustedHost"});
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                3,
                now + GONE_AFTER_MS + 4,
                json!({"agents":[forged.clone()]}),
            ),
        )
        .unwrap();
        assert_eq!(
            registered_host(
                &conn,
                "unregistered",
                &serde_json::to_value(origin(&forged)).unwrap()
            )
            .unwrap(),
            None
        );
    }

    #[tokio::test]
    async fn invalid_native_repository_reports_do_not_block_valid_v1_or_native_siblings() {
        let (database, bus) = frozen();
        let conn = database.lock().unwrap();
        let ctx = context(&bus);
        let now = db::now_ms();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,kind,canonical_remote,created_at) VALUES('p','discord','','repo','github.com/nitecon/amux',0)", []).unwrap();
        let mut invalid = agent();
        invalid["canonical_remote"] = json!("github.com/other/project");
        let legacy = json!({"session_id":"legacy","instance_id":"i","provider":"codex","os":"linux","project":"unregistered"});
        let native = json!({"session_id":"native","instance_id":"i","provider":"codex","os":"linux","project":"p","canonical_remote":"github.com/nitecon/amux"});
        heartbeat(
            &conn,
            &ctx,
            &report(
                remote::HEARTBEAT,
                1,
                now,
                json!({"agents":[invalid,legacy,native]}),
            ),
        )
        .unwrap();
        assert_eq!(agents(&conn, now).unwrap().len(), 2);
        assert!(
            targets(&conn, "p").unwrap().is_empty(),
            "event validation never grants manual routing"
        );
        conn.execute(
            "UPDATE projects SET canonical_remote='github.com/new/project' WHERE ident='p'",
            [],
        )
        .unwrap();
        let current = agents(&conn, now).unwrap();
        assert_eq!(
            current.len(),
            1,
            "native report loses validity when project binding changes"
        );
        assert_eq!(current[0].session_id, "legacy");
    }

    #[tokio::test]
    async fn heartbeat_sequence_survives_omission_and_rejects_reverse_order_and_replay() {
        let (database, bus) = frozen();
        let conn = database.lock().unwrap();
        let ctx = context(&bus);
        let old = report(remote::HEARTBEAT, 10, 10_000, json!({"agents":[agent()]}));
        let mut newer_agent = agent();
        newer_agent["instance_id"] = json!("new");
        let newer = report(
            remote::HEARTBEAT,
            20,
            20_000,
            json!({"agents":[newer_agent]}),
        );
        heartbeat(&conn, &ctx, &newer).unwrap();
        heartbeat(&conn, &ctx, &old).unwrap();
        heartbeat(&conn, &ctx, &newer).unwrap();
        let (instance, seen): (String, i64) = conn
            .query_row(
                "SELECT json_extract(agent_json,'$.instance_id'),report_seen_at FROM task_target_presence",
                [],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .unwrap();
        assert_eq!((instance.as_str(), seen), ("new", 20_000));
        let omitted = report(remote::HEARTBEAT, 30, 30_000, json!({"agents":[]}));
        heartbeat(&conn, &ctx, &omitted).unwrap();
        heartbeat(&conn, &ctx, &newer).unwrap();
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM task_target_presence", [], |row| row
                .get::<_, i64>(
                0
            ))
            .unwrap(),
            0
        );
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.online'",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            1
        );
        assert_eq!(
            conn.query_row(
                "SELECT heartbeat_seq FROM presence_sources WHERE source='client:a'",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            30
        );
    }

    #[tokio::test]
    async fn activity_sequence_fences_drafts_identity_changes_and_backwards_evaluation_time() {
        let (database, bus) = frozen();
        let conn = database.lock().unwrap();
        let ctx = context(&bus);
        // Existing rows from an older schema start with zero cursors.
        seed_report(&conn, "client:a", agent(), 0, None, false, "unknown");
        let activity_report = |seq, at, draft| {
            report(
                remote::ACTIVITY,
                seq,
                at,
                json!({"session_id":SESSION,"instance_id":"i","output_at":0,"draft":draft}),
            )
        };
        activity(&conn, &ctx, &activity_report(2, 20_000, true)).unwrap();
        activity(&conn, &ctx, &activity_report(1, 10_000, false)).unwrap();
        assert!(conn
            .query_row("SELECT draft FROM task_target_presence", [], |row| row
                .get::<_, bool>(0))
            .unwrap());
        activity(&conn, &ctx, &activity_report(3, 40_000, false)).unwrap();
        tick(&conn, &ctx, 10_000).unwrap();
        assert_eq!(
            conn.query_row("SELECT state FROM task_target_presence", [], |row| row
                .get::<_, String>(
                0
            ))
            .unwrap(),
            "idle"
        );
        let mut changed = agent();
        changed["generation"] = json!("new-execution");
        heartbeat(
            &conn,
            &ctx,
            &report(remote::HEARTBEAT, 5, 50_000, json!({"agents":[changed]})),
        )
        .unwrap();
        activity(&conn, &ctx, &activity_report(4, 45_000, true)).unwrap();
        assert_eq!(
            conn.query_row("SELECT state FROM task_target_presence", [], |row| row
                .get::<_, String>(
                0
            ))
            .unwrap(),
            "unknown"
        );
        assert_eq!(last_output(&conn, SESSION).unwrap(), None);
        activity(&conn, &ctx, &activity_report(6, 60_000, true)).unwrap();
        assert_eq!(
            conn.query_row("SELECT state FROM task_target_presence", [], |row| row
                .get::<_, String>(
                0
            ))
            .unwrap(),
            "busy"
        );
    }

    #[tokio::test]
    async fn clock_expiry_rejects_expired_replay_but_accepts_a_delayed_live_heartbeat() {
        let (database, bus) = frozen();
        let conn = database.lock().unwrap();
        let ctx = context(&bus);
        heartbeat(
            &conn,
            &ctx,
            &report(remote::HEARTBEAT, 1, 1_000, json!({"agents":[agent()]})),
        )
        .unwrap();
        tick(&conn, &ctx, 100_000).unwrap();
        heartbeat(
            &conn,
            &ctx,
            &report(remote::HEARTBEAT, 2, 2_000, json!({"agents":[agent()]})),
        )
        .unwrap();
        assert_eq!(
            agents(&conn, 100_000).unwrap().len(),
            0,
            "expired report cannot resurrect the session"
        );
        heartbeat(
            &conn,
            &ctx,
            &report(remote::HEARTBEAT, 3, 50_000, json!({"agents":[agent()]})),
        )
        .unwrap();
        assert_eq!(
            agents(&conn, 100_000).unwrap().len(),
            1,
            "a delayed but live report corrects the premature expiry"
        );
        assert_eq!(
            conn.query_row(
                "SELECT report_seen_at FROM task_target_presence",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            50_000
        );
    }

    #[tokio::test]
    async fn failed_presence_transition_does_not_advance_the_report_cursor() {
        let (database, bus) = frozen();
        database.lock().unwrap().execute_batch(
            "CREATE TRIGGER fail_online BEFORE INSERT ON router_events WHEN NEW.id='agent.online'
             BEGIN SELECT RAISE(ABORT,'event storage unavailable'); END;",
        ).unwrap();
        let ctx = context(&bus);
        let heartbeat = report(remote::HEARTBEAT, 1, 10_000, json!({"agents":[agent()]}));
        assert!(matches!(
            Track.run(&heartbeat, &ctx).await,
            StepOutcome::Fail(_)
        ));
        {
            let conn = database.lock().unwrap();
            assert_eq!(
                conn.query_row("SELECT COUNT(*) FROM presence_sources", [], |row| row
                    .get::<_, i64>(0))
                    .unwrap(),
                0
            );
            assert_eq!(
                conn.query_row("SELECT COUNT(*) FROM task_target_presence", [], |row| row
                    .get::<_, i64>(
                    0
                ))
                .unwrap(),
                0
            );
            conn.execute_batch("DROP TRIGGER fail_online").unwrap();
        }
        assert_eq!(Track.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(agents(&database.lock().unwrap(), 10_000).unwrap().len(), 1);
    }

    async fn settle(database: &db::Db) {
        for _ in 0..200 {
            let open: i64 = database
                .lock()
                .unwrap()
                .query_row(
                    "SELECT COUNT(*) FROM router_deliveries WHERE state NOT IN ('completed','stopped','skipped')",
                    [],
                    |row| row.get(0),
                )
                .unwrap();
            if open == 0 {
                return;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let errors: Vec<(String, String, Option<String>)> = database
            .lock()
            .unwrap()
            .prepare("SELECT plugin, state, last_error FROM router_deliveries WHERE state NOT IN ('completed','stopped','skipped')")
            .unwrap()
            .query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap();
        panic!("deliveries did not settle: {errors:?}");
    }

    fn emitted(database: &db::Db) -> Vec<String> {
        database
            .lock()
            .unwrap()
            .prepare("SELECT id FROM router_events WHERE id LIKE 'agent.%' ORDER BY seq")
            .unwrap()
            .query_map([], |row| row.get(0))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap()
    }

    #[test]
    fn summary_counts_machines_oses_agents_and_busy_agents() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        for (session, source, os, state) in [
            ("a", "client:one", "linux", "busy"),
            ("b", "client:one", "linux", "idle"),
            ("c", "client:two", "windows", "busy"),
            ("d", "client:three", "macos", "unknown"),
        ] {
            seed_report(
                &conn,
                source,
                json!({"session_id":session,"instance_id":"instance","project":"p","os":os}),
                db::now_ms(),
                None,
                false,
                state,
            );
        }
        assert_eq!(
            summary(&conn).unwrap(),
            json!({"machines": 3, "os": ["linux", "macos", "windows"], "agents": 4, "active": 2})
        );
    }

    #[test]
    fn controller_snapshot_excludes_expired_reports_and_checks_idle_eligibility() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        let now = 200_000;
        for (session, seen_at, output_at, draft, state) in [
            ("oldest", now, Some(now - 60_000), false, "idle"),
            ("draft", now, Some(now - 50_000), true, "idle"),
            ("recent", now, Some(now - 1), false, "idle"),
            ("unknown", now, None, false, "unknown"),
            ("expired", now - GONE_AFTER_MS, Some(0), false, "busy"),
        ] {
            seed_report(
                &conn,
                "fixture",
                json!({"session_id":session,"instance_id":"instance","project":"p"}),
                seen_at,
                output_at,
                draft,
                state,
            );
        }
        let snapshot = agents(&conn, now).unwrap();
        assert_eq!(
            snapshot.len(),
            4,
            "expired report is excluded before deletion"
        );
        let idle: Vec<_> = snapshot
            .iter()
            .filter(|agent| agent.idle(now))
            .map(|agent| agent.session_id.as_str())
            .collect();
        assert_eq!(idle, ["oldest"]);
        assert_eq!(last_output(&conn, "oldest").unwrap(), Some(now - 60_000));
        assert_eq!(last_output(&conn, "unknown").unwrap(), None);
        assert_eq!(last_output(&conn, "absent").unwrap(), None);
    }

    #[tokio::test]
    async fn heartbeats_and_activity_drive_online_idle_busy_and_gone() {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, _router) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        let now = db::now_ms();
        bus.emit(
            "client:a",
            remote::HEARTBEAT,
            1,
            json!({"agents": [agent()]}),
            None,
        )
        .await
        .unwrap();
        settle(&database).await;
        assert_eq!(
            emitted(&database),
            [ONLINE],
            "state is unknown until activity is reported"
        );
        let activity = |output_at: i64, draft: bool| json!({"session_id": SESSION, "instance_id": "i", "output_at": output_at, "draft": draft});
        bus.emit("client:a", remote::ACTIVITY, 1, activity(now, false), None)
            .await
            .unwrap();
        settle(&database).await;
        bus.emit(
            "client:a",
            remote::ACTIVITY,
            1,
            activity(now - 60_000, true),
            None,
        )
        .await
        .unwrap();
        settle(&database).await;
        assert_eq!(
            emitted(&database),
            [ONLINE, BUSY],
            "a draft keeps a quiet agent busy"
        );
        bus.emit(
            "client:a",
            remote::ACTIVITY,
            1,
            activity(now - 60_000, false),
            None,
        )
        .await
        .unwrap();
        settle(&database).await;
        // Output time only moves forward, so the stale report above did not count.
        assert_eq!(emitted(&database), [ONLINE, BUSY]);
        bus.emit("client:b", remote::ACTIVITY, 1, activity(now, true), None)
            .await
            .unwrap();
        bus.emit("clock", clock::TICK, 1, json!({}), None)
            .await
            .unwrap();
        settle(&database).await;
        assert_eq!(
            emitted(&database),
            [ONLINE, BUSY],
            "another client cannot report for this agent"
        );
        let conn = database.lock().unwrap();
        tick(&conn, &context(&bus), now + IDLE_AFTER_MS).unwrap();
        drop(conn);
        assert_eq!(emitted(&database), [ONLINE, BUSY, IDLE]);
        let conn = database.lock().unwrap();
        // Liveness counts from the heartbeat's own server time, not the test's clock.
        let seen_at: i64 = conn
            .query_row(
                "SELECT report_seen_at FROM task_target_presence",
                [],
                |row| row.get(0),
            )
            .unwrap();
        tick(&conn, &context(&bus), seen_at + GONE_AFTER_MS - 1).unwrap();
        let gone: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM router_events WHERE id=?1",
                [GONE],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(gone, 0, "not gone before the window ends");
        tick(&conn, &context(&bus), seen_at + GONE_AFTER_MS).unwrap();
        drop(conn);
        assert_eq!(emitted(&database), [ONLINE, BUSY, IDLE, GONE]);
    }

    fn context(bus: &router::Bus) -> StepContext {
        router::test_context(bus, PLUGIN)
    }
}
