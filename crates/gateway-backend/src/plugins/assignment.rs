//! Assignment: after Presence evaluates a clock tick, sweeps all reported idle
//! agents and gives each the oldest eligible task in its project. A delivery
//! the client could not make releases the claim so the task is offered again.
use crate::{
    db,
    plugins::{clock, notify, presence, tasks, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
    task_events::{self, AgentOrigin},
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection, OptionalExtension};
use serde_json::{json, Value};
use std::collections::{HashMap, HashSet};

pub const PLUGIN: &str = "assignment";
pub const COMMENT: &str = "Automatically assigned by amux after 30 seconds without output from this agent's terminal. Evaluate whether this task is still required. If a user decision is needed, record the question and move the task to User review. Close obsolete work with a reason; otherwise start execution. Respect platform requirements.";

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS assignment_offers (
            notify_uid TEXT PRIMARY KEY,
            task_id    TEXT NOT NULL,
            project    TEXT NOT NULL,
            session_id TEXT NOT NULL,
            reporter_source TEXT
        );
        CREATE INDEX IF NOT EXISTS assignment_offers_task ON assignment_offers(task_id);
        CREATE TABLE IF NOT EXISTS assignment_refusals (
            session_id TEXT PRIMARY KEY,
            output_at  INTEGER NOT NULL
        );
        CREATE TRIGGER IF NOT EXISTS assignment_offer_task_updated AFTER UPDATE ON tasks BEGIN
            DELETE FROM assignment_offers WHERE task_id=NEW.id;
        END;
        CREATE TRIGGER IF NOT EXISTS assignment_offer_task_deleted AFTER DELETE ON tasks BEGIN
            DELETE FROM assignment_offers WHERE task_id=OLD.id;
        END;
        CREATE TRIGGER IF NOT EXISTS assignment_offer_commented AFTER INSERT ON task_comments BEGIN
            DELETE FROM assignment_offers WHERE task_id=NEW.task_id;
        END;
        CREATE TRIGGER IF NOT EXISTS assignment_offer_comment_updated AFTER UPDATE ON task_comments BEGIN
            DELETE FROM assignment_offers WHERE task_id IN (OLD.task_id,NEW.task_id);
        END;
        CREATE TRIGGER IF NOT EXISTS assignment_offer_comment_deleted AFTER DELETE ON task_comments BEGIN
            DELETE FROM assignment_offers WHERE task_id=OLD.task_id;
        END;",
    )?;
    let has_source: bool = conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM pragma_table_info('assignment_offers') WHERE name='reporter_source')",
        [], |row|row.get(0),
    )?;
    if !has_source {
        // Legacy offers retain their task/row but cannot authorize a callback:
        // their original authenticated reporter was never captured.
        conn.execute_batch("ALTER TABLE assignment_offers ADD COLUMN reporter_source TEXT")?;
    }
    Ok(())
}

pub struct Assignment;

impl Plugin for Assignment {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        [clock::TICK, notify::RESULT]
            .into_iter()
            .map(|event| Subscription {
                event,
                versions: 1..=1,
                depends_on: if event == clock::TICK {
                    vec![presence::PLUGIN]
                } else {
                    Vec::new()
                },
            })
            .collect()
    }
    fn pre_exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Enabled(PLUGIN))]
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
            match event.id.as_str() {
                notify::RESULT => release(&conn, event)?,
                clock::TICK => assign(&conn, ctx, event.emitted_at)?,
                _ => {}
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

/// An offer the client did not deliver returns its task to the queue.
fn release(conn: &Connection, event: &Event) -> Result<()> {
    let Some(correlation) = event.correlation else {
        return Ok(());
    };
    // Consume the offer only if the task release and refusal both commit. A
    // failed run must retain the offer so the router can retry compensation.
    task_events::mutation(conn, None, |tx| {
        let offer: Option<(String, String)> = tx
            .query_row(
                "DELETE FROM assignment_offers WHERE notify_uid=?1 AND session_id=?2 AND reporter_source=?3
                 RETURNING task_id, session_id",
                params![correlation.to_string(),event.payload["target"].as_str(),event.source],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()?;
        let Some((task, session)) = offer else {
            return Ok(());
        };
        if event.payload["status"] == "delivered" {
            return Ok(());
        }
        // Only while the agent still holds it untouched.
        tx.execute(
            "UPDATE tasks SET status='todo', owner_agent_id=NULL, owner_origin_json=NULL,
             started_at=NULL, updated_at=?3
             WHERE id=?1 AND status='in_progress' AND json_extract(owner_origin_json,'$.session_id')=?2",
            params![task, session, db::now_ms()],
        )?;
        // The refusing session stays idle and oldest, so it would win the same
        // task every tick. Skip it until its terminal shows new output.
        if let Some(output_at) = presence::last_output(tx, &session)? {
            tx.execute(
                "INSERT OR REPLACE INTO assignment_refusals(session_id, output_at) VALUES(?1,?2)",
                params![session, output_at],
            )?;
        }
        Ok(())
    })
}

fn assign(conn: &Connection, ctx: &StepContext, now: i64) -> Result<()> {
    let agents = presence::agents(conn, now)?;
    let outputs: HashMap<_, _> = agents
        .iter()
        .map(|agent| (agent.session_id.as_str(), agent.output_at))
        .collect();
    // New output (or a departed session) ends a refusal.
    let refusals = conn
        .prepare("SELECT session_id,output_at FROM assignment_refusals")?
        .query_map([], |row| {
            Ok((row.get::<_, String>(0)?, row.get::<_, i64>(1)?))
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    let mut refused = HashSet::new();
    for (session, output_at) in refusals {
        if outputs.get(session.as_str()) == Some(&Some(output_at)) {
            refused.insert(session);
        } else {
            conn.execute(
                "DELETE FROM assignment_refusals WHERE session_id=?1",
                params![session],
            )?;
        }
    }
    for report in agents {
        if !report.idle(now) || refused.contains(&report.session_id) {
            continue;
        }
        let agent = report.agent;
        let project = agent["project"].as_str().unwrap_or_default();
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
    let labels: Vec<String> = if task["labels"].is_null() {
        Vec::new()
    } else {
        let Ok(labels) = serde_json::from_value(task["labels"].clone()) else {
            return false;
        };
        labels
    };
    let session = agent["session_id"].as_str().unwrap_or_default();
    let Ok(os) = crate::task_platform::required_os(&labels) else {
        return false;
    };
    if os.is_some_and(|os| agent["os"].as_str() != Some(os))
        || labels.contains(&format!("review-exclude:{session}"))
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
/// task store rejects the claim.
fn claim(conn: &Connection, ctx: &StepContext, agent: &Value, task: &Value) -> Result<bool> {
    let reporter_source = agent["source"].as_str().ok_or_else(|| {
        anyhow::anyhow!("assignment agent is missing its authenticated report source")
    })?;
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
        // The task store enforces OS and reviewer exclusion;
        // a rejected claim rolls back and the task is simply not eligible.
        if db::update_task(tx, project, id, &update, Some(&actor))?.is_none() {
            return Ok(false);
        }
        db::insert_comment(tx, id, "amux", "system", COMMENT)?;
        let current: String = tx.query_row(
            &format!(
                "SELECT {} FROM tasks t WHERE t.id=?1",
                task_events::task_json("t")
            ),
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
        let uid = notify::emit(tx, ctx, payload)?;
        // Insert last: any later task or comment mutation invalidates the offer
        // atomically, so a delayed delivery result cannot undo touched work.
        tx.execute(
            "INSERT INTO assignment_offers(notify_uid,task_id,project,session_id,reporter_source) VALUES(?1,?2,?3,?4,?5)",
            params![uid.to_string(), id, project, session,reporter_source],
        )?;
        Ok(true)
    });
    match claimed {
        Err(error)
            if error.is::<crate::task_dispatch::Conflict>()
                || error.is::<task_events::OwnershipConflict>() =>
        {
            tracing::debug!(
                "Assignment of {id} to {} rejected: {error}",
                origin.session_id
            );
            Ok(false)
        }
        result => result,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{plugins, router};
    use std::time::Duration;

    fn agent(session: &str) -> Value {
        json!({"session_id": session, "instance_id": "9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b0002",
               "provider": "claude", "os": "linux", "project": "p","source":"client-fixture"})
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
        assert!(eligible(
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
    async fn idle_agents_claim_queued_tasks_in_fifo_order_on_tick_only() {
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
        seed_agent(
            &database,
            agent(session),
            db::now_ms(),
            40_000,
            false,
            "busy",
        );
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
        assert_eq!(
            database
                .lock()
                .unwrap()
                .query_row(
                    "SELECT COUNT(*) FROM tasks WHERE status='in_progress'",
                    [],
                    |row| row.get::<_, i64>(0)
                )
                .unwrap(),
            0
        );
        tick(&bus, &database).await;
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

    fn seed_agent(
        database: &db::Db,
        agent: Value,
        seen_at: i64,
        quiet: i64,
        draft: bool,
        state: &str,
    ) {
        presence::seed_report(
            &database.lock().unwrap(),
            "client-fixture",
            agent,
            seen_at,
            Some(db::now_ms() - quiet),
            draft,
            state,
        );
    }

    fn setup() -> (db::Db, router::Bus, tokio::task::JoinHandle<()>) {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, worker) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        for project in ["empty", "p", "q"] {
            database.lock().unwrap().execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES(?1,'discord','',0)", params![project]).unwrap();
        }
        (database, bus, worker)
    }

    fn queue(database: &db::Db, project: &str, at: i64, labels: &[&str]) -> String {
        let id = uuid::Uuid::now_v7().to_string();
        database.lock().unwrap().execute(
            "INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,rank,labels) VALUES(?1,?2,'queued work','fixture',?3,?4,?4,?5)",
            params![id, project, at, 1000-at, serde_json::to_string(labels).unwrap()],
        ).unwrap();
        id
    }

    fn session(index: u8, project: &str) -> Value {
        let mut value = agent(&format!("9e1c7a54-2b7f-4f43-8f0e-1f6f6e1b00{index:02}"));
        value["project"] = json!(project);
        value
    }

    fn state(database: &db::Db, id: &str) -> (String, Option<String>) {
        database.lock().unwrap().query_row("SELECT status,json_extract(owner_origin_json,'$.session_id') FROM tasks WHERE id=?1", params![id], |row| Ok((row.get(0)?,row.get(1)?))).unwrap()
    }

    async fn tick(bus: &router::Bus, database: &db::Db) {
        bus.emit(
            "clock-fixture",
            clock::TICK,
            1,
            json!({"at":db::now_ms()}),
            None,
        )
        .await
        .unwrap();
        settle(database).await;
    }

    fn skipped_result(notify: &str) -> Event {
        Event {
            seq: 0,
            id: notify::RESULT.into(),
            version: 1,
            uid: uuid::Uuid::now_v7(),
            payload: json!({"target":session(1,"p")["session_id"], "status":"skipped", "reason":"busy"}),
            source: "client-fixture".into(),
            emitted_at: db::now_ms(),
            correlation: Some(notify.parse().unwrap()),
        }
    }

    #[tokio::test]
    async fn offer_receipts_require_captured_source_and_target_even_after_presence_is_gone() {
        let (database, bus, worker) = setup();
        worker.abort();
        let agent = session(1, "p");
        seed_agent(
            &database,
            agent.clone(),
            db::now_ms(),
            40_000,
            false,
            "idle",
        );
        let task = queue(&database, "p", 1, &[]);
        let ctx = router::test_context(&bus, PLUGIN);
        let conn = database.lock().unwrap();
        assert!(claim(&conn, &ctx, &agent, &queued(&conn, "p").unwrap()[0]).unwrap());
        let notify: String = conn
            .query_row("SELECT notify_uid FROM assignment_offers", [], |row| {
                row.get(0)
            })
            .unwrap();
        for status in ["delivered", "skipped", "failed"] {
            let mut result = skipped_result(&notify);
            result.payload["status"] = json!(status);
            result.source = "client:other".into();
            release(&conn, &result).unwrap();
            result.source = "client-fixture".into();
            result.payload["target"] = json!("another-session");
            release(&conn, &result).unwrap();
            assert_eq!(
                conn.query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                    .get::<_, i64>(0))
                    .unwrap(),
                1,
                "invalid {status} receipt must not consume the offer"
            );
            assert_eq!(
                conn.query_row(
                    "SELECT status FROM tasks WHERE id=?1",
                    params![task],
                    |row| row.get::<_, String>(0)
                )
                .unwrap(),
                "in_progress"
            );
            assert_eq!(
                conn.query_row("SELECT COUNT(*) FROM assignment_refusals", [], |row| row
                    .get::<_, i64>(0))
                    .unwrap(),
                0
            );
        }
        conn.execute("DELETE FROM task_target_presence", [])
            .unwrap();
        release(&conn, &skipped_result(&notify)).unwrap();
        assert_eq!(
            conn.query_row(
                "SELECT status FROM tasks WHERE id=?1",
                params![task],
                |row| row.get::<_, String>(0)
            )
            .unwrap(),
            "todo",
            "captured reporter may compensate after its live report is gone"
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0
        );
    }

    #[tokio::test]
    async fn legacy_offer_upgrade_preserves_rows_and_claims_without_granting_callback_authority() {
        let (database, bus, worker) = setup();
        let task = queue(&database, "p", 1, &[]);
        let agent = session(1, "p");
        bus.emit(
            "client-fixture",
            plugins::remote::HEARTBEAT,
            1,
            json!({"agents":[agent]}),
            None,
        )
        .await
        .unwrap();
        settle(&database).await;
        bus.emit("client-fixture",plugins::remote::ACTIVITY,1,json!({"session_id":agent["session_id"],"instance_id":agent["instance_id"],"output_at":db::now_ms()-40_000,"draft":false}),None).await.unwrap();
        settle(&database).await;
        tick(&bus, &database).await;
        worker.abort();
        let conn = database.lock().unwrap();
        let notify: String = conn
            .query_row("SELECT notify_uid FROM assignment_offers", [], |row| {
                row.get(0)
            })
            .unwrap();
        let retained_source: String = conn.query_row(
            "SELECT json_extract(p.payload,'$.agent.source') FROM router_events p,router_events n
             WHERE n.uid=?1 AND p.seq<n.seq AND p.source='plugin:presence' AND p.id='agent.idle'
               AND json_extract(p.payload,'$.agent.session_id')=?2 ORDER BY p.seq DESC LIMIT 1",
            params![notify,agent["session_id"].as_str()],|row|row.get(0),
        ).unwrap();
        assert_eq!(
            retained_source, "client-fixture",
            "HEAD-style pending offer retains authentic source transition history"
        );
        let before = serde_json::to_value(
            db::get_task_detail(&conn, "p", &task)
                .unwrap()
                .unwrap()
                .task,
        )
        .unwrap();
        conn.execute_batch("ALTER TABLE assignment_offers DROP COLUMN reporter_source")
            .unwrap();
        initialize(&conn).unwrap();
        initialize(&conn).unwrap();
        for status in ["delivered", "skipped", "failed"] {
            let mut result = skipped_result(&notify);
            result.payload["status"] = json!(status);
            release(&conn, &result).unwrap();
        }
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM assignment_offers WHERE reporter_source IS NULL",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            1
        );
        let after = serde_json::to_value(
            db::get_task_detail(&conn, "p", &task)
                .unwrap()
                .unwrap()
                .task,
        )
        .unwrap();
        assert_eq!(
            after, before,
            "migration and unprovable callbacks leave the claim intact"
        );
        // HEAD had no offer-invalidation triggers. A same-ms progress comment
        // leaves every task field equal to the notification snapshot; source
        // history alone cannot prove the old claim remained untouched.
        conn.execute_batch(
            "DROP TRIGGER assignment_offer_commented; DROP TRIGGER assignment_offer_task_updated;",
        )
        .unwrap();
        conn.execute("INSERT INTO task_comments(id,task_id,author,author_type,content,created_at)
             SELECT ?1,id,'owner','agent','Progress before upgrade',updated_at FROM tasks WHERE id=?2",
             params![uuid::Uuid::now_v7().to_string(),task]).unwrap();
        conn.execute(
            "UPDATE tasks SET updated_at=updated_at WHERE id=?1",
            params![task],
        )
        .unwrap();
        initialize(&conn).unwrap();
        release(&conn, &skipped_result(&notify)).unwrap();
        let touched = serde_json::to_value(
            db::get_task_detail(&conn, "p", &task)
                .unwrap()
                .unwrap()
                .task,
        )
        .unwrap();
        assert_eq!(
            touched, before,
            "same-ms progress has no immutable revision in a legacy offer"
        );
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM assignment_offers WHERE reporter_source IS NULL",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            1,
            "retained source history must not reauthorize a touched legacy offer"
        );
        conn.execute(
            "UPDATE tasks SET title='Progress recorded' WHERE id=?1",
            params![task],
        )
        .unwrap();
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0,
            "normal task mutation still retires legacy offers"
        );
    }

    #[tokio::test]
    async fn delayed_skip_preserves_progress_even_when_comment_timestamp_matches_claim() {
        let (database, bus, worker) = setup();
        worker.abort();
        let task = queue(&database, "p", 1, &[]);
        let ctx = router::test_context(&bus, PLUGIN);
        let conn = database.lock().unwrap();
        assert!(claim(
            &conn,
            &ctx,
            &session(1, "p"),
            &queued(&conn, "p").unwrap()[0]
        )
        .unwrap());
        let notify: String = conn
            .query_row("SELECT notify_uid FROM assignment_offers", [], |row| {
                row.get(0)
            })
            .unwrap();
        conn.execute(
            "INSERT INTO task_comments(id,task_id,author,author_type,content,created_at)
             SELECT ?1,id,'agent','agent','Working on this now',updated_at FROM tasks WHERE id=?2",
            params![uuid::Uuid::now_v7().to_string(), task],
        )
        .unwrap();
        release(&conn, &skipped_result(&notify)).unwrap();
        assert_eq!(
            conn.query_row(
                "SELECT status FROM tasks WHERE id=?1",
                params![task],
                |row| row.get::<_, String>(0)
            )
            .unwrap(),
            "in_progress"
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0
        );
    }

    #[tokio::test]
    async fn delayed_skip_cannot_release_a_new_claim_by_the_same_session() {
        let (database, bus, worker) = setup();
        worker.abort();
        let task = queue(&database, "p", 1, &[]);
        let ctx = router::test_context(&bus, PLUGIN);
        let conn = database.lock().unwrap();
        assert!(claim(
            &conn,
            &ctx,
            &session(1, "p"),
            &queued(&conn, "p").unwrap()[0]
        )
        .unwrap());
        let first: String = conn
            .query_row("SELECT notify_uid FROM assignment_offers", [], |row| {
                row.get(0)
            })
            .unwrap();
        conn.execute("UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,started_at=NULL WHERE id=?1", params![task]).unwrap();
        assert!(claim(
            &conn,
            &ctx,
            &session(1, "p"),
            &queued(&conn, "p").unwrap()[0]
        )
        .unwrap());
        let second: String = conn
            .query_row("SELECT notify_uid FROM assignment_offers", [], |row| {
                row.get(0)
            })
            .unwrap();
        assert_ne!(first, second);
        release(&conn, &skipped_result(&first)).unwrap();
        assert_eq!(
            conn.query_row(
                "SELECT status FROM tasks WHERE id=?1",
                params![task],
                |row| row.get::<_, String>(0)
            )
            .unwrap(),
            "in_progress"
        );
        assert_eq!(
            conn.query_row("SELECT notify_uid FROM assignment_offers", [], |row| row
                .get::<_, String>(
                0
            ))
            .unwrap(),
            second
        );
    }

    #[tokio::test]
    async fn claim_rejections_skip_the_task_but_storage_failures_are_retryable() {
        let (database, bus, worker) = setup();
        worker.abort();
        let task = queue(&database, "p", 1, &[]);
        let ctx = router::test_context(&bus, PLUGIN);
        let conn = database.lock().unwrap();
        let queued = queued(&conn, "p").unwrap().remove(0);
        let mut incompatible = session(1, "p");
        incompatible["os"] = json!("windows");
        conn.execute(
            "UPDATE tasks SET labels='[\"os:linux\"]' WHERE id=?1",
            params![task],
        )
        .unwrap();
        assert!(!claim(&conn, &ctx, &incompatible, &queued).unwrap());
        conn.execute_batch(
            "CREATE TRIGGER fail_offer BEFORE INSERT ON assignment_offers
             BEGIN SELECT RAISE(ABORT, 'offer storage unavailable'); END;",
        )
        .unwrap();
        let error = claim(&conn, &ctx, &session(1, "p"), &queued).unwrap_err();
        assert!(error.to_string().contains("offer storage unavailable"));
        assert_eq!(
            conn.query_row(
                "SELECT status FROM tasks WHERE id=?1",
                params![task],
                |row| row.get::<_, String>(0)
            )
            .unwrap(),
            "todo"
        );
        for table in ["assignment_offers", "task_comments"] {
            assert_eq!(
                conn.query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| row
                    .get::<_, i64>(0))
                    .unwrap(),
                0
            );
        }
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            0
        );
        conn.execute_batch("DROP TRIGGER fail_offer").unwrap();
        assert!(claim(&conn, &ctx, &session(1, "p"), &queued).unwrap());
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            1
        );
    }

    #[tokio::test]
    async fn failed_release_retains_the_offer_and_replays_compensation_atomically() {
        let (database, bus, worker) = setup();
        worker.abort();
        seed_agent(
            &database,
            session(1, "p"),
            db::now_ms(),
            40_000,
            false,
            "idle",
        );
        let task = queue(&database, "p", 1, &[]);
        let ctx = router::test_context(&bus, PLUGIN);
        let conn = database.lock().unwrap();
        assert!(claim(
            &conn,
            &ctx,
            &session(1, "p"),
            &queued(&conn, "p").unwrap()[0]
        )
        .unwrap());
        let notify: String = conn
            .query_row("SELECT notify_uid FROM assignment_offers", [], |row| {
                row.get(0)
            })
            .unwrap();
        let result = skipped_result(&notify);
        conn.execute_batch(
            "CREATE TRIGGER fail_refusal BEFORE INSERT ON assignment_refusals
             BEGIN SELECT RAISE(ABORT, 'refusal storage unavailable'); END;",
        )
        .unwrap();
        assert!(release(&conn, &result)
            .unwrap_err()
            .to_string()
            .contains("refusal storage unavailable"));
        assert_eq!(
            conn.query_row(
                "SELECT status FROM tasks WHERE id=?1",
                params![task],
                |row| row.get::<_, String>(0)
            )
            .unwrap(),
            "in_progress"
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            1
        );
        conn.execute_batch("DROP TRIGGER fail_refusal").unwrap();
        release(&conn, &result).unwrap();
        release(&conn, &result).unwrap();
        assert_eq!(
            conn.query_row(
                "SELECT status FROM tasks WHERE id=?1",
                params![task],
                |row| row.get::<_, String>(0)
            )
            .unwrap(),
            "todo"
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM assignment_refusals", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            1
        );
    }

    #[tokio::test]
    async fn one_tick_fills_all_free_sessions_across_workspaces_and_repeated_ticks_do_not_duplicate(
    ) {
        let (database, bus, _router) = setup();
        let now = db::now_ms();
        for (index, project, quiet) in [
            (1, "empty", 60_000),
            (2, "p", 50_000),
            (3, "p", 40_000),
            (4, "q", 30_100),
        ] {
            // Start busy: Presence must evaluate this same tick before Assignment.
            seed_agent(
                &database,
                session(index, project),
                now,
                quiet,
                false,
                "busy",
            );
        }
        let incompatible = queue(&database, "p", 0, &["os:windows"]);
        let third = queue(&database, "p", 3, &[]);
        let first = queue(&database, "p", 1, &["autoreview-finding"]);
        let second = queue(&database, "p", 2, &[]);
        let other = queue(&database, "q", 1, &[]);
        tick(&bus, &database).await;
        assert_eq!(
            state(&database, &first),
            (
                "in_progress".into(),
                session(2, "p")["session_id"].as_str().map(str::to_owned)
            )
        );
        assert_eq!(
            state(&database, &second),
            (
                "in_progress".into(),
                session(3, "p")["session_id"].as_str().map(str::to_owned)
            )
        );
        assert_eq!(
            state(&database, &other),
            (
                "in_progress".into(),
                session(4, "q")["session_id"].as_str().map(str::to_owned)
            )
        );
        assert_eq!(state(&database, &third).0, "todo");
        assert_eq!(state(&database, &incompatible).0, "todo");
        tick(&bus, &database).await;
        assert_eq!(
            database
                .lock()
                .unwrap()
                .query_row("SELECT COUNT(*) FROM assignment_offers", [], |row| row
                    .get::<_, i64>(0))
                .unwrap(),
            3
        );
    }

    #[tokio::test]
    async fn a_still_idle_session_picks_up_later_work_on_the_next_tick_without_another_idle_transition(
    ) {
        let (database, bus, _router) = setup();
        seed_agent(
            &database,
            session(1, "p"),
            db::now_ms(),
            40_000,
            false,
            "idle",
        );
        tick(&bus, &database).await;
        let first = queue(&database, "p", 1, &[]);
        bus.emit(
            "task-fixture",
            "task.created",
            1,
            json!({"project":"p","task":{"id":first,"status":"todo"}}),
            None,
        )
        .await
        .unwrap();
        settle(&database).await;
        assert_eq!(state(&database, &first).0, "todo");
        tick(&bus, &database).await;
        assert_eq!(state(&database, &first).0, "in_progress");
        database
            .lock()
            .unwrap()
            .execute("UPDATE tasks SET status='done' WHERE id=?1", params![first])
            .unwrap();
        let next = queue(&database, "p", 2, &[]);
        tick(&bus, &database).await;
        assert_eq!(state(&database, &next).0, "in_progress");
    }

    #[tokio::test]
    async fn tick_revalidates_busy_drafting_unknown_and_departed_sessions_without_blocking_other_workspaces(
    ) {
        let (database, bus, _router) = setup();
        let now = db::now_ms();
        seed_agent(&database, session(1, "p"), now, 40_000, false, "idle");
        let existing = queue(&database, "p", 0, &[]);
        database
            .lock()
            .unwrap()
            .execute(
                "UPDATE tasks SET status='in_progress',owner_origin_json=?2 WHERE id=?1",
                params![
                    existing,
                    json!({"session_id":session(1,"p")["session_id"]}).to_string()
                ],
            )
            .unwrap();
        seed_agent(&database, session(2, "p"), now, 40_000, true, "idle");
        seed_agent(
            &database,
            session(3, "p"),
            now - presence::GONE_AFTER_MS,
            40_000,
            false,
            "idle",
        );
        seed_agent(&database, session(4, "p"), now, 0, false, "unknown");
        database
            .lock()
            .unwrap()
            .execute(
                "UPDATE task_target_presence SET output_at=NULL WHERE session_id=?1",
                params![session(4, "p")["session_id"].as_str()],
            )
            .unwrap();
        seed_agent(&database, session(5, "p"), now, 0, false, "idle");
        seed_agent(&database, session(6, "q"), now, 40_000, false, "busy");
        let blocked = queue(&database, "p", 1, &[]);
        let available = queue(&database, "q", 1, &[]);
        tick(&bus, &database).await;
        assert_eq!(state(&database, &blocked).0, "todo");
        assert_eq!(
            state(&database, &available).1,
            session(6, "q")["session_id"].as_str().map(str::to_owned)
        );
        assert_eq!(
            database
                .lock()
                .unwrap()
                .query_row(
                    "SELECT COUNT(*) FROM task_target_presence WHERE session_id=?1 AND report_seen_at IS NOT NULL",
                    params![session(3, "p")["session_id"].as_str()],
                    |row| row.get::<_, i64>(0)
                )
                .unwrap(),
            0
        );
    }

    #[tokio::test]
    async fn a_refusing_idle_session_is_skipped_until_new_output() {
        let (database, bus, _router) = setup();
        seed_agent(
            &database,
            session(1, "p"),
            db::now_ms(),
            40_000,
            false,
            "idle",
        );
        let task = queue(&database, "p", 1, &[]);
        tick(&bus, &database).await;
        let notify: String = database
            .lock()
            .unwrap()
            .query_row("SELECT notify_uid FROM assignment_offers", [], |row| {
                row.get(0)
            })
            .unwrap();
        bus.emit("client-fixture",notify::RESULT,1,json!({"target":session(1,"p")["session_id"],"status":"skipped","reason":"draft changed"}),Some(uuid::Uuid::parse_str(&notify).unwrap())).await.unwrap();
        settle(&database).await;
        assert_eq!(state(&database, &task).0, "todo");
        let offers = |database: &db::Db| -> i64 {
            database
                .lock()
                .unwrap()
                .query_row(
                    "SELECT COUNT(*) FROM task_comments WHERE task_id=?1 AND author='amux'",
                    params![task],
                    |row| row.get(0),
                )
                .unwrap()
        };
        // The refusing session stays idle and oldest; the next tick must skip it.
        tick(&bus, &database).await;
        assert_eq!(state(&database, &task).0, "todo");
        assert_eq!(offers(&database), 1);
        seed_agent(
            &database,
            session(2, "p"),
            db::now_ms(),
            35_000,
            false,
            "busy",
        );
        tick(&bus, &database).await;
        assert_eq!(
            state(&database, &task).1,
            session(2, "p")["session_id"].as_str().map(str::to_owned)
        );
        assert_eq!(offers(&database), 2);
        // New terminal output ends the refusal.
        let refused = |database: &db::Db| -> i64 {
            database
                .lock()
                .unwrap()
                .query_row("SELECT COUNT(*) FROM assignment_refusals", [], |row| {
                    row.get(0)
                })
                .unwrap()
        };
        assert_eq!(refused(&database), 1);
        database
            .lock()
            .unwrap()
            .execute(
                "UPDATE task_target_presence SET output_at=output_at+1 WHERE session_id=?1",
                params![session(1, "p")["session_id"].as_str()],
            )
            .unwrap();
        tick(&bus, &database).await;
        assert_eq!(refused(&database), 0);
    }
}
