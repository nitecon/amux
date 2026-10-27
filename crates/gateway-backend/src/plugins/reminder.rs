//! Reminder: nudges an agent that has been idle for ten minutes while it still
//! owns in-progress work, once per task revision.
use crate::{
    plugins::{clock, notify, presence, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
    task_events,
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection};
use serde_json::{json, Value};

pub const PLUGIN: &str = "reminder";
/// Twenty idle windows, as the desktop reminder used.
pub const IDLE_FOR_MS: i64 = 20 * presence::IDLE_AFTER_MS;

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS reminder_agents (
            session_id TEXT PRIMARY KEY,
            agent_json TEXT NOT NULL,
            idle_since INTEGER NOT NULL
        );
        CREATE TABLE IF NOT EXISTS reminder_sent (
            task_id    TEXT NOT NULL,
            revision   INTEGER NOT NULL,
            PRIMARY KEY(task_id, revision)
        );",
    )?;
    Ok(())
}

pub struct Reminder;

impl Plugin for Reminder {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        [presence::IDLE, presence::BUSY, presence::GONE, clock::TICK]
            .into_iter()
            .map(|event| Subscription {
                event,
                versions: 1..=1,
                depends_on: Vec::new(),
            })
            .collect()
    }
    fn pre_exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Enabled(PLUGIN))]
    }
    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Remind)]
    }
}

struct Remind;

#[async_trait]
impl Step for Remind {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let result = (|| -> Result<()> {
            let conn = ctx.db().lock().unwrap();
            let tx = conn.unchecked_transaction()?;
            let agent = &event.payload["agent"];
            let session = agent["session_id"].as_str().unwrap_or_default();
            match event.id.as_str() {
                presence::IDLE => {
                    tx.execute(
                        "INSERT INTO reminder_agents(session_id,agent_json,idle_since) VALUES(?1,?2,?3)
                         ON CONFLICT(session_id) DO UPDATE SET agent_json=?2, idle_since=?3",
                        params![session, agent.to_string(), event.emitted_at],
                    )?;
                }
                presence::BUSY | presence::GONE => {
                    tx.execute(
                        "DELETE FROM reminder_agents WHERE session_id=?1",
                        params![session],
                    )?;
                }
                _ => remind(&tx, ctx, event.emitted_at)?,
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

fn remind(conn: &Connection, ctx: &StepContext, now: i64) -> Result<()> {
    let agents: Vec<String> = conn
        .prepare("SELECT agent_json FROM reminder_agents WHERE idle_since <= ?1")?
        .query_map(params![now - IDLE_FOR_MS], |row| row.get(0))?
        .collect::<rusqlite::Result<_>>()?;
    for agent in agents {
        let agent: Value = serde_json::from_str(&agent)?;
        let session = agent["session_id"].as_str().unwrap_or_default();
        let sql = format!(
            "SELECT {}, t.updated_at FROM tasks t WHERE t.status='in_progress'
             AND json_extract(t.owner_origin_json,'$.session_id')=?1
             AND NOT EXISTS(SELECT 1 FROM reminder_sent r WHERE r.task_id=t.id AND r.revision=t.updated_at)",
            task_events::task_json("t")
        );
        let owned: Vec<(String, i64)> = conn
            .prepare(&sql)?
            .query_map(params![session], |row| Ok((row.get(0)?, row.get(1)?)))?
            .collect::<rusqlite::Result<_>>()?;
        for (task, revision) in owned {
            let task: Value = serde_json::from_str(&task)?;
            let id = task["id"].as_str().unwrap_or_default().to_owned();
            let project = task["project_ident"].clone();
            let instructions = format!("You still own this task. Continue it, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux --agent {session} tasks user-review {id}.");
            let payload = json!({
                "target": session, "project": project, "mode": "assigned", "require_idle": true,
                "heading": "In-progress task reminder", "instructions": instructions,
                "event": {"kind": "task_created", "project_ident": project, "created_at": now,
                          "task": task, "comment": null, "origin": null},
            });
            ctx.emit_in(conn, notify::NOTIFY, 1, payload)?;
            conn.execute(
                "INSERT INTO reminder_sent(task_id,revision) VALUES(?1,?2)",
                params![id, revision],
            )?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{db, plugins, router};

    #[tokio::test]
    async fn idle_owners_are_reminded_once_per_task_revision() {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, _router) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        let ctx = router::test_context(&bus, PLUGIN);
        let session = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001";
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)", [])
            .unwrap();
        conn.execute(
            "INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_origin_json)
             VALUES('01a17e58-0000-7000-8000-000000000001','p','t','user',0,5,'in_progress',?1)",
            [json!({"session_id": session}).to_string()],
        )
        .unwrap();
        let agent = json!({"session_id": session, "project": "p"});
        conn.execute(
            "INSERT INTO reminder_agents(session_id,agent_json,idle_since) VALUES(?1,?2,1000)",
            params![session, agent.to_string()],
        )
        .unwrap();
        let reminders = |conn: &Connection| -> i64 {
            conn.query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                [],
                |row| row.get(0),
            )
            .unwrap()
        };
        remind(&conn, &ctx, 1000 + IDLE_FOR_MS - 1).unwrap();
        assert_eq!(reminders(&conn), 0, "not idle long enough");
        remind(&conn, &ctx, 1000 + IDLE_FOR_MS).unwrap();
        assert_eq!(reminders(&conn), 1);
        remind(&conn, &ctx, 1000 + 2 * IDLE_FOR_MS).unwrap();
        assert_eq!(reminders(&conn), 1, "once per revision");
        conn.execute("UPDATE tasks SET updated_at=6", []).unwrap();
        remind(&conn, &ctx, 1000 + 2 * IDLE_FOR_MS).unwrap();
        assert_eq!(reminders(&conn), 2, "a new revision is reminded again");
        let payload: String = conn
            .query_row(
                "SELECT payload FROM router_events WHERE id='agent.notify' LIMIT 1",
                [],
                |row| row.get(0),
            )
            .unwrap();
        let payload: Value = serde_json::from_str(&payload).unwrap();
        assert_eq!(payload["heading"], "In-progress task reminder");
        assert_eq!(payload["target"], session);
    }
}
