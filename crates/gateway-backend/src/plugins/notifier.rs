//! Notifier: tells the right live agents about task activity. Ported from the
//! desktop audience rules: owners hear about their tasks, explicit requesters
//! receive results, actors never hear about their own changes, and queued or
//! user-review work is left to Assignment and the user.
use crate::{
    plugins::{notify, presence, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection};
use serde_json::{json, Value};

pub const PLUGIN: &str = "notifier";
/// The task events terminals render, as before the router.
const TASK_EVENTS: [(&str, &str); 3] = [
    ("task.created", "task_created"),
    ("task.commented", "task_commented"),
    ("task.completed", "task_completed"),
];

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS notifier_agents (
            session_id TEXT PRIMARY KEY,
            project    TEXT NOT NULL,
            agent_json TEXT NOT NULL
        );",
    )?;
    Ok(())
}

pub struct Notifier;

impl Plugin for Notifier {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        [
            presence::ONLINE,
            presence::IDLE,
            presence::BUSY,
            presence::GONE,
        ]
        .into_iter()
        .chain(TASK_EVENTS.iter().map(|(id, _)| *id))
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
        vec![Box::new(Notify)]
    }
}

struct Notify;

#[async_trait]
impl Step for Notify {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let result = (|| -> Result<()> {
            let conn = ctx.db().lock().unwrap();
            let tx = conn.unchecked_transaction()?;
            if event.id.starts_with("agent.") {
                track(&tx, event)?;
            } else {
                announce(&tx, ctx, event)?;
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

fn track(conn: &Connection, event: &Event) -> Result<()> {
    let agent = &event.payload["agent"];
    let session = agent["session_id"].as_str().unwrap_or_default();
    if event.id == presence::GONE {
        conn.execute(
            "DELETE FROM notifier_agents WHERE session_id=?1",
            params![session],
        )?;
    } else {
        conn.execute(
            "INSERT INTO notifier_agents(session_id,project,agent_json) VALUES(?1,?2,?3)
             ON CONFLICT(session_id) DO UPDATE SET project=?2, agent_json=?3",
            params![session, agent["project"].as_str(), agent.to_string()],
        )?;
    }
    Ok(())
}

fn announce(conn: &Connection, ctx: &StepContext, event: &Event) -> Result<()> {
    let Some((_, kind)) = TASK_EVENTS.iter().find(|(id, _)| *id == event.id) else {
        return Ok(());
    };
    let payload = &event.payload;
    let project = payload["project"].as_str().unwrap_or_default();
    let agents: Vec<Value> = conn
        .prepare("SELECT agent_json FROM notifier_agents WHERE project=?1 ORDER BY session_id")?
        .query_map(params![project], |row| row.get::<_, String>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?
        .iter()
        .map(|agent| serde_json::from_str(agent))
        .collect::<serde_json::Result<_>>()?;
    for agent in agents
        .iter()
        .filter(|agent| recipient(kind, payload, agent))
    {
        // Requester results are informational and wait until the agent is idle.
        let notification = json!({
            "target": agent["session_id"], "project": project, "mode": "recipient",
            "require_idle": requester_result(kind, payload, agent),
            "heading": null, "instructions": null,
            "event": {"kind": kind, "project_ident": project, "created_at": event.emitted_at,
                      "task": payload["task"], "comment": payload["comment"], "origin": payload["actor"]},
        });
        ctx.emit_in(conn, notify::NOTIFY, 1, notification)?;
    }
    Ok(())
}

fn same(origin: &Value, agent: &Value) -> bool {
    origin["session_id"].is_string()
        && origin["session_id"] == agent["session_id"]
        && origin["instance_id"] == agent["instance_id"]
}

/// A comment or completion result for the explicit requester, not the owner.
fn requester_result(kind: &str, payload: &Value, agent: &Value) -> bool {
    let task = &payload["task"];
    matches!(kind, "task_commented" | "task_completed")
        && !same(&task["owner_origin"], agent)
        && same(&task["requester"]["origin"], agent)
        && task["requester"]["generation"] == agent["generation"]
}

/// Port of the desktop `Target::mismatch_reason` / `requester_result` rules.
pub fn recipient(kind: &str, payload: &Value, agent: &Value) -> bool {
    let task = &payload["task"];
    let owner = &task["owner_origin"];
    let requester = &task["requester"];
    let os: Vec<&str> = task["labels"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .filter_map(|label| label.strip_prefix("os:"))
        .collect();
    if os
        .first()
        .is_some_and(|os| agent["os"].as_str() != Some(os))
        || (!task["goal"].is_null() && task["goal"] != agent["goal"])
        || same(&payload["actor"], agent)
    {
        return false;
    }
    let result = matches!(kind, "task_commented" | "task_completed");
    if requester_result(kind, payload, agent) {
        return true;
    }
    if (result && !requester.is_null() && owner.is_null())
        || task["status"] == "user_review"
        || (task["status"] == "todo" && owner.is_null() && task["owner_agent_id"].is_null())
    {
        return false;
    }
    if kind == "task_created" && task["routing"]["mode"] == "session" {
        let target = &task["routing"]["target"];
        if !same(&target["origin"], agent) || target["generation"] != agent["generation"] {
            return false;
        }
    }
    if !owner.is_null() {
        return same(owner, agent);
    }
    task["owner_agent_id"].is_null()
        || task["owner_agent_id"] == agent["agent_id"]
        || task["owner_agent_id"] == agent["session_id"]
}

#[cfg(test)]
mod tests {
    use super::*;

    const ME: &str = "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0001";

    fn agent() -> Value {
        json!({"session_id": ME, "instance_id": "i", "os": "linux", "generation": "g", "project": "p"})
    }

    fn payload(task: Value, actor: Value) -> Value {
        let mut base = json!({"status": "in_progress", "labels": [], "goal": null, "owner_origin": null,
                              "owner_agent_id": null, "requester": null, "routing": {"mode": "auto"}});
        base.as_object_mut()
            .unwrap()
            .extend(task.as_object().unwrap().clone());
        json!({"project": "p", "task": base, "actor": actor})
    }

    #[test]
    fn audience_matches_the_desktop_rules() {
        let me = json!({"session_id": ME, "instance_id": "i"});
        let user = Value::Null;
        let owned = json!({"owner_origin": me});
        assert!(
            recipient(
                "task_commented",
                &payload(owned.clone(), user.clone()),
                &agent()
            ),
            "owner hears comments"
        );
        assert!(
            !recipient(
                "task_commented",
                &payload(owned.clone(), me.clone()),
                &agent()
            ),
            "not its own"
        );
        let other = json!({"owner_origin": {"session_id": "other", "instance_id": "x"}});
        assert!(!recipient(
            "task_commented",
            &payload(other.clone(), user.clone()),
            &agent()
        ));
        let review = json!({"owner_origin": me, "status": "user_review"});
        assert!(
            !recipient("task_commented", &payload(review, user.clone()), &agent()),
            "user decision"
        );
        let queued = json!({"status": "todo"});
        assert!(
            !recipient("task_created", &payload(queued, user.clone()), &agent()),
            "left to assignment"
        );
        let windows = json!({"owner_origin": me, "labels": ["os:windows"]});
        assert!(!recipient(
            "task_commented",
            &payload(windows, user.clone()),
            &agent()
        ));

        let requested = json!({"origin": me, "generation": "g"});
        let result = json!({"owner_origin": {"session_id": "other", "instance_id": "x"}, "requester": requested, "status": "done"});
        assert!(
            recipient(
                "task_completed",
                &payload(result.clone(), user.clone()),
                &agent()
            ),
            "requester gets results"
        );
        let completed = payload(result.clone(), user.clone());
        assert!(
            requester_result("task_completed", &completed, &agent()),
            "results wait for idle"
        );
        let comment = payload(owned.clone(), user.clone());
        assert!(
            !requester_result("task_commented", &comment, &agent()),
            "owner notices do not"
        );
        let mut stale = agent();
        stale["generation"] = json!("old");
        assert!(
            !recipient("task_completed", &payload(result, user.clone()), &stale),
            "only that execution"
        );
        let unowned = json!({"status": "done", "requester": {"origin": {"session_id": "other", "instance_id": "x"}, "generation": "g"}});
        assert!(!recipient(
            "task_completed",
            &payload(unowned, user.clone()),
            &agent()
        ));

        let reserved = json!({"status": "todo", "owner_origin": me,
                              "routing": {"mode": "session", "target": {"origin": me, "generation": "g"}}});
        assert!(recipient(
            "task_created",
            &payload(reserved.clone(), user.clone()),
            &agent()
        ));
        assert!(!recipient("task_created", &payload(reserved, user), &stale));
    }
}
