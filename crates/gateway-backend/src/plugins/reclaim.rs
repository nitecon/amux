//! Reclaim: returns claimed work to the queue after 15 minutes without task
//! activity, as the gateway always has, except while the server sees the
//! owning agent busy in its terminal.
use crate::{
    db,
    plugins::{clock, presence, tasks, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection};
#[cfg(test)]
use serde_json::json;

pub const PLUGIN: &str = "reclaim";

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch("CREATE TABLE IF NOT EXISTS reclaim_busy (session_id TEXT PRIMARY KEY);")?;
    Ok(())
}

pub struct Reclaim;

impl Plugin for Reclaim {
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
        vec![Box::new(Sweep)]
    }
}

struct Sweep;

#[async_trait]
impl Step for Sweep {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let result = (|| -> Result<()> {
            let conn = ctx.db().lock().unwrap();
            let session = event.payload["agent"]["session_id"]
                .as_str()
                .unwrap_or_default();
            match event.id.as_str() {
                presence::BUSY => {
                    conn.execute(
                        "INSERT OR IGNORE INTO reclaim_busy(session_id) VALUES(?1)",
                        params![session],
                    )?;
                }
                presence::IDLE | presence::GONE => {
                    conn.execute(
                        "DELETE FROM reclaim_busy WHERE session_id=?1",
                        params![session],
                    )?;
                }
                _ => {
                    let busy: Vec<String> = conn
                        .prepare("SELECT session_id FROM reclaim_busy")?
                        .query_map([], |row| row.get(0))?
                        .collect::<rusqlite::Result<_>>()?;
                    for project in db::all_projects(&conn)? {
                        db::reclaim_stale_tasks_except(&conn, &project.ident, &busy)?;
                    }
                }
            }
            Ok(())
        })();
        tasks::wake();
        match result {
            Ok(()) => StepOutcome::Continue,
            Err(error) => StepOutcome::Fail(error.to_string()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stale_work_is_reclaimed_unless_its_owner_is_busy() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)", [])
            .unwrap();
        for (id, owner) in [("1", "busy"), ("2", "idle")] {
            conn.execute(
                "INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_origin_json)
                 VALUES(?1,'p','t','user',0,0,'in_progress',?2)",
                params![format!("01a17e58-0000-7000-8000-00000000000{id}"), json!({"session_id": owner}).to_string()],
            )
            .unwrap();
        }
        assert_eq!(
            db::reclaim_stale_tasks_except(&conn, "p", &["busy".into()]).unwrap(),
            1
        );
        let statuses: Vec<String> = conn
            .prepare("SELECT status FROM tasks ORDER BY id")
            .unwrap()
            .query_map([], |row| row.get(0))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap();
        assert_eq!(statuses, ["in_progress", "todo"]);
    }
}
