//! Presence: derives agent liveness and idle/busy state from what clients
//! report. It consumes `client.heartbeat`, `terminal.activity` and
//! `clock.tick`, and emits `agent.online`, `agent.idle`, `agent.busy` and
//! `agent.gone` on transitions. Time is the event's own `emitted_at`.
use crate::{
    plugins::{clock, remote},
    router::{Event, EventDefinition, Plugin, Step, StepContext, StepOutcome, Subscription},
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

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS presence_agents (
            session_id TEXT PRIMARY KEY,
            source     TEXT NOT NULL,
            agent_json TEXT NOT NULL,
            seen_at    INTEGER NOT NULL,
            output_at  INTEGER,
            draft      INTEGER NOT NULL DEFAULT 0,
            state      TEXT NOT NULL DEFAULT 'unknown'
                       CHECK(state IN ('unknown','busy','idle'))
        );",
    )?;
    Ok(())
}

/// Live agents across the gateway: distinct client machines, their OSes and
/// how many agents are busy. Gone agents are already deleted.
pub fn summary(conn: &Connection) -> Result<Value> {
    let (agents, machines, active): (i64, i64, i64) = conn.query_row(
        "SELECT COUNT(*), COUNT(DISTINCT source), COALESCE(SUM(state='busy'), 0) FROM presence_agents",
        [],
        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
    )?;
    let os = conn
        .prepare(
            "SELECT DISTINCT json_extract(agent_json,'$.os') AS os FROM presence_agents
             WHERE os IS NOT NULL ORDER BY os",
        )?
        .query_map([], |row| row.get::<_, String>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(json!({"machines": machines, "os": os, "agents": agents, "active": active}))
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

fn heartbeat(conn: &Connection, ctx: &StepContext, event: &Event) -> Result<()> {
    let now = event.emitted_at;
    let mut listed = Vec::new();
    for agent in event.payload["agents"].as_array().into_iter().flatten() {
        let session = agent["session_id"].as_str().unwrap_or_default().to_owned();
        let mut agent = agent.clone();
        agent["source"] = json!(event.source);
        let previous: Option<(String, String)> = conn
            .query_row(
                "SELECT source, agent_json FROM presence_agents WHERE session_id=?1",
                params![session],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()?;
        // One client cannot take over another client's agent.
        if previous
            .as_ref()
            .is_some_and(|(source, _)| *source != event.source)
        {
            continue;
        }
        conn.execute(
            "INSERT INTO presence_agents(session_id,source,agent_json,seen_at) VALUES(?1,?2,?3,?4)
             ON CONFLICT(session_id) DO UPDATE SET agent_json=?3, seen_at=?4",
            params![session, event.source, agent.to_string(), now],
        )?;
        let changed = previous.as_ref().is_none_or(|(_, before)| {
            serde_json::from_str::<Value>(before).ok().as_ref() != Some(&agent)
        });
        if changed {
            if previous.is_some() {
                // A new execution or changed identity: restart its state.
                conn.execute(
                    "UPDATE presence_agents SET state='unknown', output_at=NULL, draft=0 WHERE session_id=?1",
                    params![session],
                )?;
            }
            ctx.emit_in(conn, ONLINE, 1, json!({ "agent": agent }))?;
        }
        evaluate(conn, ctx, &session, now)?;
        listed.push(session);
    }
    let departed: Vec<String> = conn
        .prepare("SELECT session_id FROM presence_agents WHERE source=?1")?
        .query_map(params![event.source], |row| row.get(0))?
        .collect::<rusqlite::Result<Vec<String>>>()?
        .into_iter()
        .filter(|session| !listed.contains(session))
        .collect();
    for session in departed {
        gone(conn, ctx, &session)?;
    }
    Ok(())
}

fn activity(conn: &Connection, ctx: &StepContext, event: &Event) -> Result<()> {
    let payload = &event.payload;
    let session = payload["session_id"].as_str().unwrap_or_default();
    let known: Option<String> = conn
        .query_row(
            "SELECT agent_json FROM presence_agents WHERE session_id=?1 AND source=?2",
            params![session, event.source],
            |row| row.get(0),
        )
        .optional()?;
    let Some(agent) = known else {
        // Reported before its heartbeat, or by another client: the next report catches up.
        return Ok(());
    };
    let agent: Value = serde_json::from_str(&agent)?;
    if agent["instance_id"] != payload["instance_id"] {
        return Ok(());
    }
    conn.execute(
        "UPDATE presence_agents SET output_at=MAX(COALESCE(output_at,?2),COALESCE(?2,output_at)),
         draft=?3 WHERE session_id=?1",
        params![
            session,
            payload["output_at"].as_i64(),
            payload["draft"].as_bool() == Some(true)
        ],
    )?;
    evaluate(conn, ctx, session, event.emitted_at)
}

fn tick(conn: &Connection, ctx: &StepContext, now: i64) -> Result<()> {
    let sessions: Vec<(String, i64)> = conn
        .prepare("SELECT session_id, seen_at FROM presence_agents")?
        .query_map([], |row| Ok((row.get(0)?, row.get(1)?)))?
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

/// Emit `agent.idle` / `agent.busy` when the derived state changes.
fn evaluate(conn: &Connection, ctx: &StepContext, session: &str, now: i64) -> Result<()> {
    let (agent, output_at, draft, state): (String, Option<i64>, bool, String) = conn.query_row(
        "SELECT agent_json, output_at, draft, state FROM presence_agents WHERE session_id=?1",
        params![session],
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
        "UPDATE presence_agents SET state=?2 WHERE session_id=?1",
        params![session, next],
    )?;
    let id = if next == "idle" { IDLE } else { BUSY };
    let agent: Value = serde_json::from_str(&agent)?;
    ctx.emit_in(conn, id, 1, json!({ "agent": agent }))?;
    Ok(())
}

fn gone(conn: &Connection, ctx: &StepContext, session: &str) -> Result<()> {
    let agent: String = conn.query_row(
        "DELETE FROM presence_agents WHERE session_id=?1 RETURNING agent_json",
        params![session],
        |row| row.get(0),
    )?;
    let agent: Value = serde_json::from_str(&agent)?;
    ctx.emit_in(conn, GONE, 1, json!({ "agent": agent }))?;
    Ok(())
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
            conn.execute(
                "INSERT INTO presence_agents(session_id,source,agent_json,seen_at,state) VALUES(?1,?2,?3,0,?4)",
                params![session, source, json!({"os": os}).to_string(), state],
            )
            .unwrap();
        }
        assert_eq!(
            summary(&conn).unwrap(),
            json!({"machines": 3, "os": ["linux", "macos", "windows"], "agents": 4, "active": 2})
        );
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
            .query_row("SELECT seen_at FROM presence_agents", [], |row| row.get(0))
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
