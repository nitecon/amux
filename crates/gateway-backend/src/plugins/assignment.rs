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

pub const PLUGIN: &str = "assignment";
pub const COMMENT: &str = "Automatically assigned by amux after 30 seconds without output from this agent's terminal. Evaluate whether this task is still required. If a user decision is needed, record the question and move the task to User review. Close obsolete work with a reason; otherwise start execution. Respect platform requirements.";

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS assignment_offers (
            notify_uid TEXT PRIMARY KEY,
            task_id    TEXT NOT NULL,
            project    TEXT NOT NULL,
            session_id TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS assignment_refusals (
            session_id TEXT PRIMARY KEY,
            output_at  INTEGER NOT NULL
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
    let offer: Option<(String, String, String)> = conn
        .query_row(
            "DELETE FROM assignment_offers WHERE notify_uid=?1 RETURNING task_id, project, session_id",
            params![correlation.to_string()],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
        )
        .optional()?;
    let Some((task, _project, session)) = offer else {
        return Ok(());
    };
    if event.payload["status"] == "delivered" {
        return Ok(());
    }
    // Only while the agent still holds it untouched.
    task_events::mutation(conn, None, |tx| {
        Ok(tx.execute(
            "UPDATE tasks SET status='todo', owner_agent_id=NULL, owner_origin_json=NULL,
             started_at=NULL, updated_at=?3
             WHERE id=?1 AND status='in_progress' AND json_extract(owner_origin_json,'$.session_id')=?2",
            params![task, session, db::now_ms()],
        )?)
    })?;
    // The refusing session stays idle and oldest, so it would win the same task
    // every tick. Skip it until its terminal shows new output.
    conn.execute(
        "INSERT OR REPLACE INTO assignment_refusals(session_id, output_at)
         SELECT session_id, output_at FROM presence_agents
         WHERE session_id=?1 AND output_at IS NOT NULL",
        params![session],
    )?;
    Ok(())
}

fn assign(conn: &Connection, ctx: &StepContext, now: i64) -> Result<()> {
    // New output (or a departed session) ends a refusal.
    conn.execute(
        "DELETE FROM assignment_refusals WHERE NOT EXISTS (
             SELECT 1 FROM presence_agents p
             WHERE p.session_id=assignment_refusals.session_id
               AND p.output_at=assignment_refusals.output_at)",
        [],
    )?;
    let agents: Vec<Value> = conn
        .prepare(
            "SELECT agent_json FROM presence_agents
             WHERE state='idle' AND draft=0 AND output_at IS NOT NULL
               AND ?1-output_at >= ?2 AND ?1-seen_at < ?3
               AND session_id NOT IN (SELECT session_id FROM assignment_refusals)
             ORDER BY output_at, session_id",
        )?
        .query_map(
            params![now, presence::IDLE_AFTER_MS, presence::GONE_AFTER_MS],
            |row| row.get::<_, String>(0),
        )?
        .collect::<rusqlite::Result<Vec<_>>>()?
        .iter()
        .map(|agent| serde_json::from_str(agent))
        .collect::<serde_json::Result<_>>()?;
    for agent in agents {
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
        database.lock().unwrap().execute(
            "INSERT INTO presence_agents(session_id,source,agent_json,seen_at,output_at,draft,state) VALUES(?1,'client-fixture',?2,?3,?4,?5,?6)",
            params![agent["session_id"].as_str(), agent.to_string(), seen_at, db::now_ms()-quiet, draft, state],
        ).unwrap();
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
                "UPDATE presence_agents SET output_at=NULL WHERE session_id=?1",
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
                    "SELECT COUNT(*) FROM presence_agents WHERE session_id=?1",
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
                "UPDATE presence_agents SET output_at=output_at+1 WHERE session_id=?1",
                params![session(1, "p")["session_id"].as_str()],
            )
            .unwrap();
        tick(&bus, &database).await;
        assert_eq!(refused(&database), 0);
    }
}
