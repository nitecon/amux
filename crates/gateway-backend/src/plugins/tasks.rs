//! Task event source. Triggers write every task change to an outbox in the
//! mutation's own transaction; a drain worker emits them as `task.*` events.
//! The legacy-compatible `task_events` journal is left untouched.
use crate::{
    router::{Bus, EventDefinition},
    task_events::{comment_json, task_json},
};
use anyhow::Result;
use rusqlite::{params, Connection};
use serde_json::{json, Value};
use std::{sync::LazyLock, time::Duration};
use tokio::{sync::Notify, task::spawn_blocking};

/// Event suffix after `task.` for each outbox kind.
pub const KINDS: [&str; 9] = [
    "created",
    "updated",
    "commented",
    "assigned",
    "started",
    "user_review",
    "completed",
    "cancelled",
    "unclaimed",
];
const BATCH: i64 = 200;
static WAKE: LazyLock<Notify> = LazyLock::new(Notify::new);

/// Drain the outbox now; called after task mutations commit.
pub fn wake() {
    WAKE.notify_one();
}

pub fn events() -> Vec<EventDefinition> {
    KINDS
        .iter()
        .map(|kind| EventDefinition {
            id: id(kind),
            version: 1,
            remote_emit: false,
            validate: task_payload,
        })
        .collect()
}

fn id(kind: &str) -> &'static str {
    match kind {
        "created" => "task.created",
        "updated" => "task.updated",
        "commented" => "task.commented",
        "assigned" => "task.assigned",
        "started" => "task.started",
        "user_review" => "task.user_review",
        "completed" => "task.completed",
        "cancelled" => "task.cancelled",
        _ => "task.unclaimed",
    }
}

fn task_payload(payload: &Value) -> std::result::Result<(), String> {
    if payload["task"].is_object() && payload["project"].is_string() {
        Ok(())
    } else {
        Err("payload needs a project and a task object".into())
    }
}

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS router_task_outbox (
            id         INTEGER PRIMARY KEY AUTOINCREMENT,
            kind       TEXT NOT NULL,
            project    TEXT NOT NULL,
            task_json  TEXT NOT NULL,
            comment_json TEXT,
            actor_json TEXT,
            created_at INTEGER NOT NULL
        );",
    )?;
    let actor = "(SELECT origin_json FROM task_mutation_context WHERE id=1)";
    let now = "CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)";
    let insert = |kind: &str, task: &str, comment: &str| {
        format!(
            "INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
             VALUES ('{kind}',NEW.project_ident,{task},{comment},{actor},{now});"
        )
    };
    let task = task_json("NEW");
    let status = |name: &str, status: &str| {
        format!(
            "CREATE TRIGGER router_task_{name} AFTER UPDATE OF status ON tasks
             WHEN NEW.status='{status}' AND OLD.status!='{status}' BEGIN {} END;",
            insert(name, &task, "NULL")
        )
    };
    let mut sql = String::new();
    sql += &format!(
        "CREATE TRIGGER router_task_created AFTER INSERT ON tasks BEGIN {} END;",
        insert("created", &task, "NULL")
    );
    sql += &format!(
        "CREATE TRIGGER router_task_updated AFTER UPDATE ON tasks BEGIN {} END;",
        insert("updated", &task, "NULL")
    );
    sql += &format!(
        "CREATE TRIGGER router_task_commented AFTER INSERT ON task_comments BEGIN
            INSERT INTO router_task_outbox(kind,project,task_json,comment_json,actor_json,created_at)
            SELECT 'commented',t.project_ident,{},{},{actor},{now} FROM tasks t WHERE t.id=NEW.task_id;
         END;",
        task_json("t"),
        comment_json("NEW")
    );
    sql += &format!(
        "CREATE TRIGGER router_task_assigned AFTER UPDATE OF owner_agent_id,owner_origin_json ON tasks
         WHEN (NEW.owner_agent_id IS NOT NULL OR NEW.owner_origin_json IS NOT NULL)
           AND (NEW.owner_agent_id IS NOT OLD.owner_agent_id
             OR NEW.owner_origin_json IS NOT OLD.owner_origin_json) BEGIN {} END;",
        insert("assigned", &task, "NULL")
    );
    sql += &status("started", "in_progress");
    sql += &status("user_review", "user_review");
    sql += &status("completed", "done");
    sql += &status("cancelled", "cancelled");
    sql += &format!(
        "CREATE TRIGGER router_task_unclaimed AFTER UPDATE OF status,owner_agent_id,owner_origin_json ON tasks
         WHEN NEW.status='todo' AND NEW.owner_agent_id IS NULL AND NEW.owner_origin_json IS NULL
           AND (OLD.owner_agent_id IS NOT NULL OR OLD.owner_origin_json IS NOT NULL
             OR OLD.status='in_progress') BEGIN {} END;",
        insert("unclaimed", &task, "NULL")
    );
    let tx = conn.unchecked_transaction()?;
    drop_triggers(&tx)?;
    tx.execute_batch(&sql)?;
    tx.commit()?;
    Ok(())
}

/// Table rebuilds drop these first; `initialize` recreates them.
pub fn drop_triggers(conn: &Connection) -> Result<()> {
    for kind in KINDS {
        conn.execute_batch(&format!("DROP TRIGGER IF EXISTS router_task_{kind};"))?;
    }
    Ok(())
}

/// Move outbox rows into the bus. Returns how many were emitted.
pub fn drain(bus: &Bus, conn: &Connection) -> Result<usize> {
    let tx = conn.unchecked_transaction()?;
    let rows = {
        let mut statement = tx.prepare(
            "SELECT id,kind,project,task_json,comment_json,actor_json FROM router_task_outbox
             ORDER BY id LIMIT ?1",
        )?;
        let rows = statement.query_map(params![BATCH], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, String>(2)?,
                row.get::<_, String>(3)?,
                row.get::<_, Option<String>>(4)?,
                row.get::<_, Option<String>>(5)?,
            ))
        })?;
        rows.collect::<rusqlite::Result<Vec<_>>>()?
    };
    let parse = |raw: Option<String>| -> Result<Value> {
        Ok(raw
            .map(|raw| serde_json::from_str(&raw))
            .transpose()?
            .unwrap_or(Value::Null))
    };
    for (_, kind, project, task, comment, actor) in &rows {
        let payload = json!({
            "project": project,
            "task": serde_json::from_str::<Value>(task)?,
            "comment": parse(comment.clone())?,
            "actor": parse(actor.clone())?,
        });
        bus.emit_in(&tx, "tasks", id(kind), 1, payload, None)?;
    }
    if let Some((last, ..)) = rows.last() {
        tx.execute(
            "DELETE FROM router_task_outbox WHERE id <= ?1",
            params![last],
        )?;
    }
    tx.commit()?;
    Ok(rows.len())
}

pub fn spawn(bus: Bus) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        loop {
            let drainer = bus.clone();
            let drained = spawn_blocking(move || {
                let conn = drainer.db().lock().unwrap();
                drain(&drainer, &conn)
            })
            .await;
            match drained {
                Ok(Ok(count)) if count > 0 => {
                    bus.wake();
                    if count as i64 == BATCH {
                        continue;
                    }
                }
                Ok(Ok(_)) => {}
                Ok(Err(error)) => tracing::warn!("Task event drain failed: {error}"),
                Err(error) => tracing::warn!("Task event drain task failed: {error}"),
            }
            tokio::select! {
                _ = WAKE.notified() => {}
                _ = tokio::time::sleep(Duration::from_secs(1)) => {}
            }
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{db, plugins::remote::Remote, router};

    fn kinds(conn: &Connection) -> Vec<String> {
        let mut kinds: Vec<String> = conn
            .prepare("SELECT kind FROM router_task_outbox")
            .unwrap()
            .query_map([], |row| row.get(0))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap();
        conn.execute("DELETE FROM router_task_outbox", []).unwrap();
        kinds.sort();
        kinds
    }

    #[tokio::test]
    async fn every_task_change_is_queued_once_and_drained_into_the_bus() {
        let database = db::open(":memory:").unwrap();
        let remote = Remote::default();
        let forwarded = events().iter().map(|event| event.id).collect();
        let mut definitions = events();
        definitions.extend(crate::plugins::remote::events());
        let (bus, _router) = router::start(
            database.clone(),
            definitions,
            vec![remote.plugin(forwarded)],
        )
        .unwrap();
        let conn = database.lock().unwrap();
        conn.execute(
            "INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)",
            [],
        )
        .unwrap();
        let task = |id: &str| {
            conn.execute(
                "INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at)
                 VALUES(?1,'p','t','user',0,0)",
                [id],
            )
            .unwrap();
        };
        let update = |sql: &str| conn.execute(sql, []).unwrap();
        task("a");
        assert_eq!(kinds(&conn), ["created"]);
        update("UPDATE tasks SET owner_agent_id='s', status='in_progress' WHERE id='a'");
        assert_eq!(kinds(&conn), ["assigned", "started", "updated"]);
        update("UPDATE tasks SET status='todo', owner_agent_id=NULL WHERE id='a'");
        assert_eq!(kinds(&conn), ["unclaimed", "updated"]);
        update("UPDATE tasks SET status='user_review' WHERE id='a'");
        assert_eq!(kinds(&conn), ["updated", "user_review"]);
        update("UPDATE tasks SET title='renamed' WHERE id='a'");
        assert_eq!(kinds(&conn), ["updated"]);
        update(
            "INSERT INTO task_comments(id,task_id,author,author_type,content,created_at)
             VALUES('c','a','user','user','hi',0)",
        );
        assert_eq!(kinds(&conn), ["commented"]);
        update("UPDATE tasks SET status='done' WHERE id='a'");
        assert_eq!(kinds(&conn), ["completed", "updated"]);
        task("b");
        update("UPDATE tasks SET status='cancelled' WHERE id='b'");
        assert_eq!(kinds(&conn), ["cancelled", "created", "updated"]);

        update("UPDATE tasks SET title='again' WHERE id='a'");
        assert_eq!(drain(&bus, &conn).unwrap(), 1);
        assert_eq!(drain(&bus, &conn).unwrap(), 0, "drained rows are removed");
        let (id, payload): (String, String) = conn
            .query_row("SELECT id,payload FROM router_events", [], |row| {
                Ok((row.get(0)?, row.get(1)?))
            })
            .unwrap();
        assert_eq!(id, "task.updated");
        let payload: Value = serde_json::from_str(&payload).unwrap();
        assert_eq!(payload["project"], "p");
        assert_eq!(payload["task"]["title"], "again");
        assert_eq!(payload["comment"], Value::Null);
    }
}
