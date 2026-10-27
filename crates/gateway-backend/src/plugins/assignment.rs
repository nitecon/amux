//! Assignment: gives each idle agent the oldest eligible queued task in its
//! project, claims it as that agent, and asks the agent to start. A delivery
//! the client could not make releases the claim so the task is offered again.
use crate::{
    db,
    plugins::{notify, presence, tasks},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
    task_events::{self, AgentOrigin},
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection, OptionalExtension};
use serde_json::{json, Value};

pub const PLUGIN: &str = "assignment";
pub const COMMENT: &str = "Automatically assigned by amux after 30 seconds without output from this agent's terminal. Evaluate whether this task is still required. If a user decision is needed, record the question and move the task to User review. Close obsolete work with a reason; otherwise start execution. Respect platform requirements.";

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS assignment_agents (
            session_id TEXT PRIMARY KEY,
            project    TEXT NOT NULL,
            agent_json TEXT NOT NULL,
            idle       INTEGER NOT NULL DEFAULT 0,
            idle_since INTEGER
        );
        CREATE TABLE IF NOT EXISTS assignment_offers (
            notify_uid TEXT PRIMARY KEY,
            task_id    TEXT NOT NULL,
            project    TEXT NOT NULL,
            session_id TEXT NOT NULL
        );",
    )?;
    Ok(())
}

pub struct Assignment;

impl Plugin for Assignment {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        [
            presence::ONLINE,
            presence::IDLE,
            presence::BUSY,
            presence::GONE,
            "task.created",
            "task.unclaimed",
            "task.user_review",
            "task.completed",
            "task.cancelled",
            notify::RESULT,
        ]
        .into_iter()
        .map(|event| Subscription {
            event,
            versions: 1..=1,
            depends_on: Vec::new(),
        })
        .collect()
    }
    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Assign)]
    }
}

struct Assign;

#[async_trait]
impl Step for Assign {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let result = (|| -> Result<()> {
            let conn = ctx.db().lock().unwrap();
            let project = match event.id.as_str() {
                notify::RESULT => release(&conn, event)?,
                id if id.starts_with("agent.") => track(&conn, event)?,
                _ => event.payload["project"].as_str().map(str::to_owned),
            };
            if let Some(project) = project {
                assign(&conn, ctx, &project)?;
            }
            Ok(())
        })();
        ctx.wake();
        tasks::wake();
        match result {
            Ok(()) => StepOutcome::Continue,
            Err(error) => StepOutcome::Fail(error.to_string()),
        }
    }
}

/// Keep this plugin's own view of agents; returns the project to assign in.
fn track(conn: &Connection, event: &Event) -> Result<Option<String>> {
    let agent = &event.payload["agent"];
    let session = agent["session_id"].as_str().unwrap_or_default();
    let project = agent["project"].as_str().unwrap_or_default();
    match event.id.as_str() {
        presence::GONE => {
            conn.execute(
                "DELETE FROM assignment_agents WHERE session_id=?1",
                params![session],
            )?;
            return Ok(None);
        }
        presence::IDLE => conn.execute(
            "INSERT INTO assignment_agents(session_id,project,agent_json,idle,idle_since)
             VALUES(?1,?2,?3,1,?4) ON CONFLICT(session_id) DO UPDATE
             SET project=?2, agent_json=?3, idle=1, idle_since=?4",
            params![session, project, agent.to_string(), event.emitted_at],
        )?,
        _ => conn.execute(
            "INSERT INTO assignment_agents(session_id,project,agent_json,idle)
             VALUES(?1,?2,?3,0) ON CONFLICT(session_id) DO UPDATE
             SET project=?2, agent_json=?3, idle=0, idle_since=NULL",
            params![session, project, agent.to_string()],
        )?,
    };
    Ok(Some(project.to_owned()))
}

/// An offer the client did not deliver returns its task to the queue.
fn release(conn: &Connection, event: &Event) -> Result<Option<String>> {
    let Some(correlation) = event.correlation else {
        return Ok(None);
    };
    let offer: Option<(String, String, String)> = conn
        .query_row(
            "DELETE FROM assignment_offers WHERE notify_uid=?1 RETURNING task_id, project, session_id",
            params![correlation.to_string()],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
        )
        .optional()?;
    let Some((task, project, session)) = offer else {
        return Ok(None);
    };
    if event.payload["status"] == "delivered" {
        return Ok(None);
    }
    // Only while the agent still holds it untouched.
    let released = task_events::mutation(conn, None, |tx| {
        Ok(tx.execute(
            "UPDATE tasks SET status='todo', owner_agent_id=NULL, owner_origin_json=NULL,
             started_at=NULL, updated_at=?3
             WHERE id=?1 AND status='in_progress' AND json_extract(owner_origin_json,'$.session_id')=?2",
            params![task, session, db::now_ms()],
        )?)
    })?;
    if released > 0 {
        // The agent did not take it, so do not offer it the same task again at once.
        conn.execute(
            "UPDATE assignment_agents SET idle=0 WHERE session_id=?1",
            params![session],
        )?;
    }
    Ok(Some(project))
}

fn assign(conn: &Connection, ctx: &StepContext, project: &str) -> Result<()> {
    let agents: Vec<Value> = conn
        .prepare(
            "SELECT agent_json FROM assignment_agents WHERE project=?1 AND idle=1
             ORDER BY idle_since, session_id",
        )?
        .query_map(params![project], |row| row.get::<_, String>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?
        .iter()
        .map(|agent| serde_json::from_str(agent))
        .collect::<serde_json::Result<_>>()?;
    for agent in agents {
        let session = agent["session_id"].as_str().unwrap_or_default();
        let busy: bool = conn.query_row(
            "SELECT EXISTS(SELECT 1 FROM tasks WHERE status='in_progress'
             AND json_extract(owner_origin_json,'$.session_id')=?1)",
            params![session],
            |row| row.get(0),
        )?;
        if busy {
            continue;
        }
        for task in queued(conn, project)? {
            if eligible(&task, &agent) && claim(conn, ctx, &agent, &task)? {
                break;
            }
        }
    }
    Ok(())
}

/// Queued tasks in FIFO order.
fn queued(conn: &Connection, project: &str) -> Result<Vec<Value>> {
    let sql = format!(
        "SELECT {} FROM tasks t WHERE t.project_ident=?1 AND t.status='todo'
         ORDER BY t.created_at, t.id",
        task_events::task_json("t")
    );
    let rows = conn
        .prepare(&sql)?
        .query_map(params![project], |row| row.get::<_, String>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(rows
        .iter()
        .map(|row| serde_json::from_str(row))
        .collect::<serde_json::Result<_>>()?)
}

/// Port of the desktop backlog eligibility rules.
pub fn eligible(task: &Value, agent: &Value) -> bool {
    let labels: Vec<&str> = task["labels"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .collect();
    let session = agent["session_id"].as_str().unwrap_or_default();
    let os = labels
        .iter()
        .filter_map(|label| label.strip_prefix("os:"))
        .collect::<Vec<_>>();
    if os.len() > 1
        || os
            .first()
            .is_some_and(|os| agent["os"].as_str() != Some(os))
        || labels.contains(&format!("review-exclude:{session}").as_str())
        || labels.contains(&"autoreview-finding")
        || task["status"] != "todo"
        || (task["kind"] == "delegated" && task["delegated_to_task_id"].is_string())
        || (!task["goal"].is_null() && task["goal"] != agent["goal"])
    {
        return false;
    }
    let owned = !task["owner_origin"].is_null() || !task["owner_agent_id"].is_null();
    if owned
        && task["owner_origin"]["session_id"].as_str() != Some(session)
        && task["owner_agent_id"].as_str() != Some(session)
        && (agent["agent_id"].is_null() || task["owner_agent_id"] != agent["agent_id"])
    {
        return false;
    }
    match task["routing"]["mode"].as_str() {
        Some("session") => {
            let target = &task["routing"]["target"];
            target["origin"]["session_id"].as_str() == Some(session)
                && target["origin"]["instance_id"] == agent["instance_id"]
                && target["generation"] == agent["generation"]
        }
        _ => true,
    }
}

/// Claim as the agent, record why, and ask it to start. Returns false when the
/// task store rejects the claim (e.g. a held review finding).
fn claim(conn: &Connection, ctx: &StepContext, agent: &Value, task: &Value) -> Result<bool> {
    let origin = AgentOrigin {
        session_id: agent["session_id"].as_str().unwrap_or_default().into(),
        instance_id: agent["instance_id"].as_str().unwrap_or_default().into(),
        provider: agent["provider"].as_str().unwrap_or_default().into(),
        os: agent["os"].as_str().unwrap_or_default().into(),
    };
    let project = task["project_ident"].as_str().unwrap_or_default();
    let id = task["id"].as_str().unwrap_or_default();
    let actor = agent["agent_id"]
        .as_str()
        .unwrap_or(&origin.session_id)
        .to_owned();
    let claimed = task_events::mutation(conn, Some(&origin), |tx| -> Result<bool> {
        let update = db::TaskUpdate {
            goal: None,
            status: Some("in_progress"),
            owner_agent_id: None,
            rank: None,
            title: None,
            description: None,
            details: None,
            labels: None,
            hostname: None,
        };
        // The task store enforces OS, reviewer exclusion and held findings;
        // a rejected claim rolls back and the task is simply not eligible.
        db::update_task(tx, project, id, &update, Some(&actor))?
            .ok_or_else(|| anyhow::anyhow!("task {id} disappeared"))?;
        db::insert_comment(tx, id, "amux", "system", COMMENT)?;
        let current: String = tx.query_row(
            &format!("SELECT {} FROM tasks t WHERE t.id=?1", task_events::task_json("t")),
            params![id],
            |row| row.get(0),
        )?;
        let current: Value = serde_json::from_str(&current)?;
        let review = current["labels"]
            .as_array()
            .is_some_and(|labels| labels.iter().any(|label| label == "task-review"));
        let session = &origin.session_id;
        let (heading, instructions) = if review {
            ("Queued independent review", "This independent review task is already claimed by your amux identity. Review the source task, associated comments and actual changes according to this task's specification. Follow KISS/YAGNI. Review only: do not edit code or implement findings. Record confirmed defects as ordinary queued tasks, post results on the source and review tasks, then mark this review task done.".to_owned())
        } else {
            ("Queued task review", format!("This task is already claimed by your amux identity. Check current code/comments and platform requirements; implement if needed, or close it with a reason if obsolete. If waiting for a user decision, comment with your question then run: amux --agent {session} tasks user-review {id}."))
        };
        let payload = json!({
            "target": session, "project": project, "mode": "assigned", "require_idle": true,
            "heading": heading, "instructions": instructions,
            "event": {"kind": "task_created", "project_ident": project, "created_at": db::now_ms(),
                      "task": current, "comment": null, "origin": null},
        });
        let uid = ctx.emit_in(tx, notify::NOTIFY, 1, payload)?;
        tx.execute(
            "INSERT INTO assignment_offers(notify_uid,task_id,project,session_id) VALUES(?1,?2,?3,?4)",
            params![uid.to_string(), id, project, session],
        )?;
        Ok(true)
    })
    .unwrap_or_else(|error| {
        tracing::debug!("Assignment of {id} to {} rejected: {error}", origin.session_id);
        false
    });
    if claimed {
        conn.execute(
            "UPDATE assignment_agents SET idle=0 WHERE session_id=?1",
            params![origin.session_id],
        )?;
    }
    Ok(claimed)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{plugins, router};
    use std::time::Duration;

    fn agent(session: &str) -> Value {
        json!({"session_id": session, "instance_id": "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0002",
               "provider": "claude", "os": "linux", "project": "p"})
    }

    async fn settle(database: &db::Db) {
        for _ in 0..300 {
            let open: Vec<(String, String, Option<String>)> = database
                .lock()
                .unwrap()
                .prepare("SELECT plugin, state, last_error FROM router_deliveries WHERE state NOT IN ('completed','stopped','skipped')")
                .unwrap()
                .query_map([], |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)))
                .unwrap()
                .collect::<rusqlite::Result<_>>()
                .unwrap();
            if open.is_empty() {
                return;
            }
            if open.iter().any(|(_, state, _)| state == "retrying") {
                panic!("delivery failed: {open:?}");
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("deliveries did not settle");
    }

    #[test]
    fn eligibility_matches_the_desktop_backlog_rules() {
        let me = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001";
        let agent = agent(me);
        let task = |extra: Value| {
            let mut task = json!({"id": "t", "project_ident": "p", "status": "todo", "kind": "normal",
                                  "labels": [], "goal": null, "owner_origin": null, "owner_agent_id": null,
                                  "routing": {"mode": "auto"}});
            task.as_object_mut()
                .unwrap()
                .extend(extra.as_object().unwrap().clone());
            task
        };
        assert!(eligible(&task(json!({})), &agent));
        assert!(eligible(&task(json!({"labels": ["os:linux"]})), &agent));
        assert!(!eligible(&task(json!({"labels": ["os:windows"]})), &agent));
        assert!(!eligible(
            &task(json!({"labels": [format!("review-exclude:{me}")]})),
            &agent
        ));
        assert!(!eligible(
            &task(json!({"labels": ["autoreview-finding"]})),
            &agent
        ));
        assert!(!eligible(&task(json!({"status": "in_progress"})), &agent));
        assert!(!eligible(
            &task(json!({"kind": "delegated", "delegated_to_task_id": "x"})),
            &agent
        ));
        assert!(!eligible(&task(json!({"goal": "release"})), &agent));
        let mut goal_agent = agent.clone();
        goal_agent["goal"] = json!("release");
        assert!(eligible(&task(json!({"goal": "release"})), &goal_agent));
        assert!(!eligible(
            &task(json!({"owner_origin": {"session_id": "someone"}})),
            &agent
        ));
        assert!(
            eligible(&task(json!({"owner_origin": {"session_id": me}})), &agent),
            "returned to me"
        );
        let target = json!({"origin": {"session_id": me, "instance_id": agent["instance_id"]}, "generation": "g"});
        assert!(!eligible(
            &task(json!({"routing": {"mode": "session", "target": target}})),
            &agent
        ));
        let mut generation = agent.clone();
        generation["generation"] = json!("g");
        assert!(eligible(
            &task(json!({"routing": {"mode": "session", "target": target}})),
            &generation
        ));
    }

    #[tokio::test]
    async fn idle_agents_claim_queued_tasks_in_fifo_order() {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, _router) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        {
            let conn = database.lock().unwrap();
            conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)", [])
                .unwrap();
            for (id, at) in [("b", 2), ("a", 1)] {
                conn.execute(
                    "INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES(?1,'p','t','user',?2,?2)",
                    params![format!("01a17e58-0000-7000-8000-00000000000{id}"), at],
                )
                .unwrap();
            }
        }
        let session = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001";
        bus.emit(
            "test",
            presence::IDLE,
            1,
            json!({"agent": agent(session)}),
            None,
        )
        .await
        .unwrap();
        settle(&database).await;
        let conn = database.lock().unwrap();
        let (status, owner): (String, Option<String>) = conn
            .query_row(
                "SELECT status, json_extract(owner_origin_json,'$.session_id') FROM tasks WHERE id LIKE '%a'",
                [],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .unwrap();
        assert_eq!(
            (status.as_str(), owner.as_deref()),
            ("in_progress", Some(session))
        );
        let notified: i64 = conn
            .query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(notified, 1);
    }
}
