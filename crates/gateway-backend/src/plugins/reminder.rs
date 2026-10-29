//! Reminder: asks the connected owner of in-progress work for an update when
//! the task has had none for ten minutes, and again every ten minutes after.
use crate::{
    plugins::{clock, notify, presence, remote, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
    task_events,
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection};
use serde_json::{json, Value};
use std::collections::HashSet;

pub const PLUGIN: &str = "reminder";
/// A task change or comment resets the wait.
pub const QUIET_FOR_MS: i64 = 10 * 60_000;

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS reminder_nudges (
            task_id TEXT PRIMARY KEY,
            sent_at INTEGER NOT NULL
        );
        CREATE TABLE IF NOT EXISTS reminder_restarts (
            source TEXT NOT NULL,
            session_id TEXT NOT NULL,
            instance_id TEXT NOT NULL,
            client_run TEXT NOT NULL,
            PRIMARY KEY(source,session_id,instance_id)
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
        [clock::TICK, remote::HEARTBEAT]
            .into_iter()
            .map(|event| Subscription {
                event,
                versions: 1..=1,
                depends_on: vec![presence::PLUGIN],
            })
            .collect()
    }
    fn pre_exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(PeriodicEnabled)]
    }
    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Remind)]
    }
}

struct PeriodicEnabled;

#[async_trait]
impl Step for PeriodicEnabled {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        if event.id == remote::HEARTBEAT {
            // Startup continuity was historically governed by the desktop's
            // automatic-notifications setting, independently of periodic nudges.
            StepOutcome::Continue
        } else {
            Enabled(PLUGIN).run(event, ctx).await
        }
    }
}

struct Remind;

#[async_trait]
impl Step for Remind {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let result = (|| -> Result<()> {
            let conn = ctx.db().lock().unwrap();
            let tx = conn.unchecked_transaction()?;
            if event.id == remote::HEARTBEAT {
                restart(&tx, ctx, event)?;
            } else {
                remind(&tx, ctx, event.emitted_at)?;
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

fn execution_key<'a>(origin: &'a Value, project: &'a Value) -> Option<(&'a str, &'a str, &'a str)> {
    Some((
        origin["session_id"].as_str()?,
        origin["instance_id"].as_str()?,
        project.as_str()?,
    ))
}

/// A client reports its lifetime; task ownership and notification scheduling
/// remain on the server. Each exact execution is considered once per lifetime.
fn restart(conn: &Connection, ctx: &StepContext, event: &Event) -> Result<()> {
    let Some(client_run) = event.payload["client_run"].as_str() else {
        return Ok(());
    };
    if !presence::heartbeat_is_current(conn, event)? {
        return Ok(());
    }
    let now = crate::db::now_ms();
    let listed: HashSet<_> = event.payload["agents"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|agent| execution_key(agent, &agent["project"]))
        .collect();
    for report in presence::agents(conn, now)? {
        let agent = report.agent;
        let Some((session, instance, project)) = execution_key(&agent, &agent["project"]) else {
            continue;
        };
        // An unresolved observation must not consume restart continuity before
        // the same execution reports its bound project.
        if project.is_empty() {
            continue;
        }
        if agent["source"] != event.source || !listed.contains(&(session, instance, project)) {
            continue;
        }
        let first = conn.execute(
            "INSERT INTO reminder_restarts(source,session_id,instance_id,client_run) VALUES(?1,?2,?3,?4)
             ON CONFLICT(source,session_id,instance_id) DO UPDATE SET client_run=excluded.client_run
             WHERE client_run!=excluded.client_run",
            params![event.source, session, instance, client_run],
        )?;
        if first == 0 {
            continue;
        }
        let sql = format!(
            "SELECT {} FROM tasks t WHERE project_ident=?1 AND status='in_progress'
             AND json_extract(owner_origin_json,'$.session_id')=?2",
            task_events::task_json("t")
        );
        let owned = conn
            .prepare(&sql)?
            .query_map(params![project, session], |row| row.get::<_, String>(0))?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        for task in owned {
            let task: Value = serde_json::from_str(&task)?;
            let id = task["id"].as_str().unwrap_or_default();
            let instructions = if task["owner_origin"]["instance_id"].as_str() == Some(instance) {
                format!(
                    "You still own this task. Continue it, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux tasks user-review {id}."
                )
            } else {
                "This terminal's previous execution still owns this task. Inspect its prior progress. This is an informational continuity reminder, not an ownership handoff. Use the existing recovery or reassignment flow before attempting task mutations or resuming execution.".to_owned()
            };
            notify::emit(
                conn,
                ctx,
                json!({
                    "target": session, "project": project, "mode": "recipient", "require_idle": true,
                    "receiver": {"session_id": session, "instance_id": instance, "project": project},
                    "heading": "In-progress task reminder", "instructions": instructions,
                    "event": {"kind": "task_created", "project_ident": project, "created_at": now,
                              "sender_name": "amux", "task": task, "comment": null, "origin": null},
                }),
            )?;
            conn.execute(
                "INSERT INTO reminder_nudges(task_id,sent_at) VALUES(?1,?2)
                 ON CONFLICT(task_id) DO UPDATE SET sent_at=excluded.sent_at",
                params![id, now],
            )?;
        }
    }
    Ok(())
}

fn remind(conn: &Connection, ctx: &StepContext, now: i64) -> Result<()> {
    let reports = presence::agents(conn, now)?;
    let connected: HashSet<_> = reports
        .iter()
        .filter_map(|report| execution_key(&report.agent, &report.agent["project"]))
        .collect();
    // Only exact live owners; a replacement execution or another project
    // cannot receive instructions for a prior owner's work.
    let sql = format!(
        "SELECT {} FROM tasks t
         LEFT JOIN reminder_nudges n ON n.task_id=t.id
         WHERE t.status='in_progress' AND ?1 - MAX(t.updated_at, COALESCE(n.sent_at, 0)) >= ?2",
        task_events::task_json("t")
    );
    let quiet: Vec<String> = conn
        .prepare(&sql)?
        .query_map(params![now, QUIET_FOR_MS], |row| row.get(0))?
        .collect::<rusqlite::Result<_>>()?;
    for task in quiet {
        let task: Value = serde_json::from_str(&task)?;
        let Some(owner) = execution_key(&task["owner_origin"], &task["project_ident"])
            .filter(|owner| connected.contains(owner))
        else {
            continue;
        };
        let session = owner.0;
        let id = task["id"].as_str().unwrap_or_default().to_owned();
        let project = task["project_ident"].clone();
        let instructions = format!(
            "You still own this task and it has had no update for 10 minutes. Post a progress comment saying where you are, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux tasks user-review {id}."
        );
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
    async fn restart_and_periodic_reminders_require_exact_owners_and_restart_is_transactional() {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, worker) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        worker.abort();
        let ctx = router::test_context(&bus, PLUGIN);
        let now = db::now_ms();
        let agent = json!({"session_id":"session", "instance_id":"instance", "project":"p",
                          "provider":"codex","os":"linux", "source":"client"});
        {
            let conn = database.lock().unwrap();
            for project in ["p", "other"] {
                conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES(?1,'discord','',0)", params![project]).unwrap();
            }
            for (suffix, project, status, instance) in [
                (1, "p", "in_progress", "instance"),
                (2, "p", "done", "instance"),
                (3, "other", "in_progress", "instance"),
                (4, "p", "in_progress", "replaced"),
            ] {
                let id = format!("01a17e58-0000-7000-8000-{suffix:012}");
                let owner = json!({"session_id":"session","instance_id":instance,"provider":"codex","os":"linux"});
                conn.execute("INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_origin_json) VALUES(?1,?2,'t','user',0,?3,?4,?5)", params![id,project,now - QUIET_FOR_MS,status,owner.to_string()]).unwrap();
            }
            presence::seed_report(&conn, "client", agent.clone(), now, None, false, "unknown");
            conn.execute(
                "INSERT INTO presence_sources(source,heartbeat_seq) VALUES('client',10)",
                [],
            )
            .unwrap();
            conn.execute("UPDATE presence_clock SET observed_at=?1", params![now])
                .unwrap();
        }
        {
            let conn = database.lock().unwrap();
            remind(&conn, &ctx, now).unwrap();
            let notices: i64 = conn
                .query_row(
                    "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                    [],
                    |row| row.get(0),
                )
                .unwrap();
            assert_eq!(
                notices, 1,
                "periodic reminders reject replaced instances, other projects and finished work"
            );
            conn.execute("DELETE FROM router_events", []).unwrap();
            conn.execute("DELETE FROM reminder_nudges", []).unwrap();
        }
        let mut heartbeat = Event {
            seq: 10,
            id: remote::HEARTBEAT.into(),
            version: 1,
            uid: uuid::Uuid::now_v7(),
            payload: json!({"agents":[agent.clone()]}),
            source: "client".into(),
            emitted_at: now,
            correlation: None,
        };
        let notices = || {
            database
                .lock()
                .unwrap()
                .query_row(
                    "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                    [],
                    |row| row.get::<_, i64>(0),
                )
                .unwrap()
        };
        db::set_setting(
            &database.lock().unwrap(),
            "plugin.reminder.enabled",
            "false",
        )
        .unwrap();
        assert_eq!(
            PeriodicEnabled.run(&heartbeat, &ctx).await,
            StepOutcome::Continue
        );
        let mut tick = heartbeat.clone();
        tick.id = clock::TICK.into();
        assert_eq!(PeriodicEnabled.run(&tick, &ctx).await, StepOutcome::Stop);
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(notices(), 0, "old clients keep periodic behavior");
        heartbeat.payload["client_run"] = json!(uuid::Uuid::now_v7().to_string());
        heartbeat.seq = 9;
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            0,
            "reordered heartbeat cannot start a client lifetime"
        );
        heartbeat.seq = 10;
        database.lock().unwrap().execute_batch(
            "CREATE TRIGGER fail_restart BEFORE INSERT ON router_events WHEN NEW.id='agent.notify'
             BEGIN SELECT RAISE(ABORT,'injected notification failure'); END;",
        ).unwrap();
        assert!(matches!(
            Remind.run(&heartbeat, &ctx).await,
            StepOutcome::Fail(_)
        ));
        {
            let conn = database.lock().unwrap();
            assert_eq!(
                conn.query_row("SELECT COUNT(*) FROM reminder_restarts", [], |row| row
                    .get::<_, i64>(0))
                    .unwrap(),
                0
            );
            conn.execute_batch("DROP TRIGGER fail_restart").unwrap();
        }
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            2,
            "startup continuity includes a previous instance in the same project"
        );
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(notices(), 2, "replay is idempotent");
        // A newer heartbeat from the same lifetime is a reconnect, not a restart.
        heartbeat.seq = 11;
        database
            .lock()
            .unwrap()
            .execute("UPDATE presence_sources SET heartbeat_seq=11", [])
            .unwrap();
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(notices(), 2);
        let listed = heartbeat.payload["agents"].clone();
        heartbeat.payload["agents"] = json!([]);
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        heartbeat.payload["agents"] = listed;
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            2,
            "omitting and readding within one lifetime must not repeat"
        );
        let old = heartbeat.clone();
        heartbeat.seq = 12;
        heartbeat.payload["client_run"] = json!(uuid::Uuid::now_v7().to_string());
        database
            .lock()
            .unwrap()
            .execute("UPDATE presence_sources SET heartbeat_seq=12", [])
            .unwrap();
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(notices(), 4, "a new desktop lifetime gets a fresh reminder");
        assert_eq!(Remind.run(&old, &ctx).await, StepOutcome::Continue);
        assert_eq!(notices(), 4, "old lifetime cannot replace the current one");
        {
            let conn = database.lock().unwrap();
            let payload:String=conn.query_row("SELECT payload FROM router_events WHERE id='agent.notify' AND json_extract(payload,'$.event.task.owner_origin.instance_id')='replaced' LIMIT 1",[],|row|row.get(0)).unwrap();
            let payload: Value = serde_json::from_str(&payload).unwrap();
            assert_eq!(payload["receiver"]["instance_id"], "instance");
            assert_eq!(
                payload["event"]["task"]["owner_origin"]["instance_id"],
                "replaced"
            );
            assert!(payload["instructions"]
                .as_str()
                .unwrap()
                .contains("not an ownership handoff"));
            let actor = task_events::AgentOrigin {
                session_id: "session".into(),
                instance_id: "instance".into(),
                provider: "codex".into(),
                os: "linux".into(),
            };
            let update = db::TaskUpdate {
                goal: None,
                status: Some("done"),
                owner_agent_id: None,
                rank: None,
                title: None,
                description: None,
                details: None,
                labels: None,
                hostname: None,
            };
            let result = task_events::mutation(&conn, Some(&actor), |tx| {
                db::update_task(
                    tx,
                    "p",
                    "01a17e58-0000-7000-8000-000000000004",
                    &update,
                    Some("session"),
                )
            });
            assert!(
                result.unwrap_err().is::<task_events::OwnershipConflict>(),
                "continuity cannot grant mutation rights over the previous execution"
            );
        }
        // Two bearer clients legitimately share a source. One client's run
        // must not clear another client's startup deduplication.
        let mut other = agent.clone();
        other["session_id"] = json!("other-session");
        let mut other_heartbeat = heartbeat.clone();
        other_heartbeat.seq = 13;
        other_heartbeat.payload =
            json!({"agents":[other.clone()],"client_run":uuid::Uuid::now_v7().to_string()});
        {
            let conn = database.lock().unwrap();
            presence::seed_report(&conn, "client", other, now, None, false, "unknown");
            conn.execute("UPDATE presence_sources SET heartbeat_seq=13", [])
                .unwrap();
        }
        assert_eq!(
            Remind.run(&other_heartbeat, &ctx).await,
            StepOutcome::Continue
        );
        heartbeat.seq = 14;
        database
            .lock()
            .unwrap()
            .execute("UPDATE presence_sources SET heartbeat_seq=14", [])
            .unwrap();
        assert_eq!(Remind.run(&heartbeat, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            4,
            "other client runs do not reissue this client's notices"
        );
        let conn = database.lock().unwrap();
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM reminder_restarts", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            2,
            "each client execution retains its own deduplication"
        );
    }

    #[tokio::test]
    async fn unresolved_observation_does_not_consume_restart_continuity_before_project_binding() {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, worker) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        worker.abort();
        let ctx = router::test_context(&bus, PLUGIN);
        let now = db::now_ms();
        let mut agent = json!({"session_id":"session","instance_id":"instance","provider":"codex","os":"windows","project":""});
        let mut event = Event {
            seq: 10,
            id: remote::HEARTBEAT.into(),
            version: 1,
            uid: uuid::Uuid::now_v7(),
            payload: json!({"agents":[agent.clone()],"client_run":uuid::Uuid::now_v7().to_string()}),
            source: "client:windows".into(),
            emitted_at: now,
            correlation: None,
        };
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)",[]).unwrap();
        conn.execute("INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_origin_json) VALUES('task','p','t','user',0,?1,'in_progress',?2)",params![now,json!({"session_id":"session","instance_id":"instance","provider":"codex","os":"windows"}).to_string()]).unwrap();
        conn.execute(
            "INSERT INTO presence_sources(source,heartbeat_seq) VALUES('client:windows',10)",
            [],
        )
        .unwrap();
        conn.execute("UPDATE presence_clock SET observed_at=?1", params![now])
            .unwrap();
        presence::seed_report(
            &conn,
            &event.source,
            agent.clone(),
            now,
            None,
            false,
            "unknown",
        );
        restart(&conn, &ctx, &event).unwrap();
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM reminder_restarts", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0,
            "an observation without a project cannot consume startup deduplication"
        );
        agent["project"] = json!("p");
        presence::seed_report(
            &conn,
            &event.source,
            agent.clone(),
            now,
            None,
            false,
            "unknown",
        );
        event.payload["agents"] = json!([agent]);
        restart(&conn, &ctx, &event).unwrap();
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            1,
            "binding the same execution later still emits its startup reminder"
        );
    }

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
                params![id, json!({"session_id": owner,"instance_id":"instance"}).to_string()],
            )
            .unwrap();
        }
        // Busy or idle does not matter; only a connected owner is nudged.
        presence::seed_report(
            &conn,
            "client",
            json!({"session_id":session,"instance_id":"instance","project":"p"}),
            0,
            None,
            false,
            "busy",
        );
        let nudges = |conn: &Connection| -> i64 {
            conn.query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                [],
                |row| row.get(0),
            )
            .unwrap()
        };
        let remind_connected = |now| {
            conn.execute(
                "UPDATE task_target_presence SET report_seen_at=?1",
                params![now],
            )
            .unwrap();
            remind(&conn, &ctx, now).unwrap();
        };
        remind_connected(1000 + QUIET_FOR_MS - 1);
        assert_eq!(nudges(&conn), 0, "updated recently");
        remind_connected(1000 + QUIET_FOR_MS);
        assert_eq!(nudges(&conn), 1, "only the connected owner");
        remind_connected(1000 + 2 * QUIET_FOR_MS - 1);
        assert_eq!(nudges(&conn), 1, "not again within ten minutes");
        remind_connected(1000 + 2 * QUIET_FOR_MS);
        assert_eq!(nudges(&conn), 2, "still quiet: nudged again");
        conn.execute(
            "UPDATE tasks SET updated_at=?1",
            params![1000 + 2 * QUIET_FOR_MS + 5],
        )
        .unwrap();
        remind_connected(1000 + 3 * QUIET_FOR_MS);
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

    #[tokio::test]
    async fn expired_owner_is_not_nudged_even_before_presence_deletes_the_report() {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, worker) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        worker.abort();
        let ctx = router::test_context(&bus, PLUGIN);
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)", []).unwrap();
        conn.execute("INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_origin_json) VALUES('01a17e58-0000-7000-8000-000000000001','p','t','user',0,0,'in_progress','{\"session_id\":\"expired\",\"instance_id\":\"instance\"}')", []).unwrap();
        let now = QUIET_FOR_MS;
        presence::seed_report(
            &conn,
            "fixture",
            json!({"session_id":"expired","instance_id":"instance","project":"p"}),
            now - presence::GONE_AFTER_MS,
            None,
            false,
            "busy",
        );
        remind(&conn, &ctx, now).unwrap();
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                [],
                |row| row.get::<_, i64>(0)
            )
            .unwrap(),
            0
        );
        assert_eq!(Reminder.subscriptions()[0].depends_on, [presence::PLUGIN]);
    }
}
