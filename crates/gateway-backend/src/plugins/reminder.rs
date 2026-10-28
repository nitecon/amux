//! Reminder: asks the connected owner of in-progress work for an update when
//! the task has had none for ten minutes, and again every ten minutes after.
use crate::{
    plugins::{clock, notify, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
    task_events,
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection};
use serde_json::{json, Value};

pub const PLUGIN: &str = "reminder";
/// A task change or comment resets the wait.
pub const QUIET_FOR_MS: i64 = 10 * 60_000;

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS reminder_nudges (
            task_id TEXT PRIMARY KEY,
            sent_at INTEGER NOT NULL
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
        vec![Subscription {
            event: clock::TICK,
            versions: 1..=1,
            depends_on: Vec::new(),
        }]
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
            remind(&tx, ctx, event.emitted_at)?;
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
    // Only owners Presence still sees connected; quiet since the later of the
    // last task update and the last nudge.
    let sql = format!(
        "SELECT {}, p.session_id FROM tasks t
         JOIN presence_agents p ON p.session_id=json_extract(t.owner_origin_json,'$.session_id')
         LEFT JOIN reminder_nudges n ON n.task_id=t.id
         WHERE t.status='in_progress' AND ?1 - MAX(t.updated_at, COALESCE(n.sent_at, 0)) >= ?2",
        task_events::task_json("t")
    );
    let quiet: Vec<(String, String)> = conn
        .prepare(&sql)?
        .query_map(params![now, QUIET_FOR_MS], |row| {
            Ok((row.get(0)?, row.get(1)?))
        })?
        .collect::<rusqlite::Result<_>>()?;
    for (task, session) in quiet {
        let task: Value = serde_json::from_str(&task)?;
        let id = task["id"].as_str().unwrap_or_default().to_owned();
        let project = task["project_ident"].clone();
        let instructions = format!("You still own this task and it has had no update for 10 minutes. Post a progress comment saying where you are, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux --agent {session} tasks user-review {id}.");
        let payload = json!({
            "target": session, "project": project, "mode": "assigned", "require_idle": true,
            "heading": "Task update requested", "instructions": instructions,
            "event": {"kind": "task_created", "project_ident": project, "created_at": now,
                      "task": task, "comment": null, "origin": null},
        });
        notify::emit(conn, ctx, payload)?;
        conn.execute(
            "INSERT INTO reminder_nudges(task_id,sent_at) VALUES(?1,?2)
             ON CONFLICT(task_id) DO UPDATE SET sent_at=?2",
            params![id, now],
        )?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{db, plugins, router};

    #[tokio::test]
    async fn quiet_in_progress_work_is_nudged_every_ten_minutes_until_it_is_updated() {
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
        for (id, owner) in [
            ("01a17e58-0000-7000-8000-000000000001", session),
            ("01a17e58-0000-7000-8000-000000000002", "departed"),
        ] {
            conn.execute(
                "INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_origin_json)
                 VALUES(?1,'p','t','user',0,1000,'in_progress',?2)",
                params![id, json!({"session_id": owner}).to_string()],
            )
            .unwrap();
        }
        // Busy or idle does not matter; only a connected owner is nudged.
        conn.execute(
            "INSERT INTO presence_agents(session_id,source,agent_json,seen_at,state) VALUES(?1,'client','{}',0,'busy')",
            params![session],
        )
        .unwrap();
        let nudges = |conn: &Connection| -> i64 {
            conn.query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                [],
                |row| row.get(0),
            )
            .unwrap()
        };
        remind(&conn, &ctx, 1000 + QUIET_FOR_MS - 1).unwrap();
        assert_eq!(nudges(&conn), 0, "updated recently");
        remind(&conn, &ctx, 1000 + QUIET_FOR_MS).unwrap();
        assert_eq!(nudges(&conn), 1, "only the connected owner");
        remind(&conn, &ctx, 1000 + 2 * QUIET_FOR_MS - 1).unwrap();
        assert_eq!(nudges(&conn), 1, "not again within ten minutes");
        remind(&conn, &ctx, 1000 + 2 * QUIET_FOR_MS).unwrap();
        assert_eq!(nudges(&conn), 2, "still quiet: nudged again");
        conn.execute(
            "UPDATE tasks SET updated_at=?1",
            params![1000 + 2 * QUIET_FOR_MS + 5],
        )
        .unwrap();
        remind(&conn, &ctx, 1000 + 3 * QUIET_FOR_MS).unwrap();
        assert_eq!(nudges(&conn), 2, "an update resets the wait");
        let payload: String = conn
            .query_row(
                "SELECT payload FROM router_events WHERE id='agent.notify' LIMIT 1",
                [],
                |row| row.get(0),
            )
            .unwrap();
        let payload: Value = serde_json::from_str(&payload).unwrap();
        assert_eq!(payload["heading"], "Task update requested");
        assert_eq!(payload["target"], session);
    }
}
