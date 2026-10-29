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
use rusqlite::Connection;
#[cfg(test)]
use serde_json::json;

pub const PLUGIN: &str = "reclaim";

pub fn initialize(conn: &Connection) -> Result<()> {
    // Presence now supplies authoritative state; discard the obsolete projection.
    conn.execute_batch("DROP TABLE IF EXISTS reclaim_busy;")?;
    Ok(())
}

pub struct Reclaim;

impl Plugin for Reclaim {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        vec![Subscription {
            event: clock::TICK,
            versions: 1..=1,
            depends_on: vec![presence::PLUGIN],
        }]
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
            let busy: Vec<_> = presence::agents(&conn, event.emitted_at)?
                .into_iter()
                .filter(|agent| agent.state == "busy")
                .map(|agent| agent.session_id)
                .collect();
            for project in db::all_projects(&conn)? {
                db::reclaim_stale_tasks_except(&conn, &project.ident, &busy)?;
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
    use crate::{plugins, router};
    use rusqlite::params;

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

    #[tokio::test]
    async fn tick_reads_committed_presence_without_waiting_for_transition_events() {
        let database = db::open(":memory:").unwrap();
        let (bus, worker) = router::start(
            database.clone(),
            plugins::events(),
            vec![Box::new(presence::Presence), Box::new(Reclaim)],
        )
        .unwrap();
        let now = db::now_ms();
        {
            let conn = database.lock().unwrap();
            conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)", []).unwrap();
            conn.execute("INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_origin_json) VALUES('01a17e58-0000-7000-8000-000000000001','p','t','user',0,0,'in_progress','{\"session_id\":\"agent\"}')", []).unwrap();
            presence::seed_report(
                &conn,
                "fixture",
                json!({"session_id":"agent","instance_id":"instance","project":"p"}),
                now,
                Some(now),
                false,
                "idle",
            );
        }
        for (quiet, expected) in [(0, "in_progress"), (presence::IDLE_AFTER_MS + 1, "todo")] {
            database
                .lock()
                .unwrap()
                .execute(
                    "UPDATE task_target_presence SET output_at=?1",
                    params![now - quiet],
                )
                .unwrap();
            bus.emit("clock-fixture", clock::TICK, 1, json!({}), None)
                .await
                .unwrap();
            let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(5);
            loop {
                let open: i64 = database.lock().unwrap().query_row("SELECT COUNT(*) FROM router_deliveries WHERE state NOT IN ('completed','stopped','skipped')", [], |row| row.get(0)).unwrap();
                if open == 0 {
                    break;
                }
                assert!(
                    tokio::time::Instant::now() < deadline,
                    "tick did not settle"
                );
                tokio::time::sleep(std::time::Duration::from_millis(10)).await;
            }
            let conn = database.lock().unwrap();
            assert_eq!(
                conn.query_row("SELECT status FROM tasks", [], |row| row
                    .get::<_, String>(0))
                    .unwrap(),
                expected
            );
            assert_eq!(
                conn.query_row(
                    "SELECT COUNT(*) FROM router_events WHERE id IN ('agent.busy','agent.idle')",
                    [],
                    |row| row.get::<_, i64>(0)
                )
                .unwrap(),
                0,
                "transition events have no subscribers; reclaim must read Presence directly"
            );
        }
        worker.abort();
    }
}
