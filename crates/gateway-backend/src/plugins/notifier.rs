//! Notifier: tells the right live agents about task activity. Ported from the
//! desktop audience rules: owners hear about their tasks, explicit requesters
//! receive results, actors never hear about their own changes, and queued or
//! user-review work is left to Assignment and the user.
use crate::{
    db,
    plugins::{notify, presence, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::Connection;
use serde_json::{json, Value};

pub const PLUGIN: &str = "notifier";
/// The task events terminals render, as before the router.
const TASK_EVENTS: [(&str, &str); 3] = [
    ("task.created", "task_created"),
    ("task.commented", "task_commented"),
    ("task.completed", "task_completed"),
];

pub fn initialize(conn: &Connection) -> Result<()> {
    // Presence owns live identity; discard the legacy unordered projection.
    conn.execute_batch(
        "DROP TABLE IF EXISTS notifier_agents;
         CREATE TABLE IF NOT EXISTS notifier_processed (
            event_seq INTEGER PRIMARY KEY REFERENCES router_events(seq) ON DELETE CASCADE
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
        TASK_EVENTS
            .iter()
            .map(|(id, _)| *id)
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
            announce(&tx, ctx, event)?;
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

fn announce(conn: &Connection, ctx: &StepContext, event: &Event) -> Result<()> {
    let Some((_, kind)) = TASK_EVENTS.iter().find(|(id, _)| *id == event.id) else {
        return Ok(());
    };
    // Notification emission and this receipt share the caller's transaction.
    // A restart after commit but before router completion must not emit again.
    if conn.execute(
        "INSERT OR IGNORE INTO notifier_processed(event_seq) VALUES(?1)",
        [event.seq],
    )? == 0
    {
        return Ok(());
    }
    let payload = &event.payload;
    let project = payload["project"].as_str().unwrap_or_default();
    let agents = presence::agents(conn, db::now_ms())?;
    for agent in agents
        .iter()
        .map(|report| &report.agent)
        .filter(|agent| agent["project"] == project)
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
        notify::emit(conn, ctx, notification)?;
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
    let labels: Vec<String> = if task["labels"].is_null() {
        Vec::new()
    } else {
        let Ok(labels) = serde_json::from_value(task["labels"].clone()) else {
            return false;
        };
        labels
    };
    let Ok(os) = crate::task_platform::required_os(&labels) else {
        return false;
    };
    if os.is_some_and(|os| agent["os"].as_str() != Some(os))
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
    use crate::{plugins, router};
    use rusqlite::params;

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

    fn persist(conn: &Connection, mut event: Event) -> Event {
        event.uid = uuid::Uuid::now_v7();
        conn.execute(
            "INSERT INTO router_events(uid,id,version,payload,source,emitted_at)
             VALUES(?1,?2,?3,?4,?5,?6)",
            params![
                event.uid.to_string(),
                event.id,
                event.version,
                event.payload.to_string(),
                event.source,
                event.emitted_at
            ],
        )
        .unwrap();
        event.seq = conn.last_insert_rowid();
        event
    }

    #[tokio::test]
    async fn announcements_use_current_presence_after_disable_identity_change_and_departure() {
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
        {
            let conn = database.lock().unwrap();
            presence::seed_report(&conn, "client", agent(), now, None, false, "unknown");
            // The old projection is removed during initialization, including
            // upgrades where it contains a departed or replaced execution.
            conn.execute_batch("CREATE TABLE notifier_agents(session_id TEXT); INSERT INTO notifier_agents VALUES('departed');").unwrap();
            initialize(&conn).unwrap();
        }
        let mut event = Event {
            seq: 0,
            id: "task.commented".into(),
            version: 1,
            uid: uuid::Uuid::nil(),
            payload: payload(
                json!({"owner_origin": {"session_id": ME, "instance_id": "i"}}),
                Value::Null,
            ),
            source: "tasks".into(),
            emitted_at: now,
            correlation: None,
        };
        event = persist(&database.lock().unwrap(), event);
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
        assert_eq!(Notify.run(&event, &ctx).await, StepOutcome::Continue);
        assert_eq!(notices(), 1, "Presence alone supplies recipients");
        {
            let conn = database.lock().unwrap();
            db::set_setting(&conn, "plugin.notifier.enabled", "false").unwrap();
            let mut replacement = agent();
            replacement["instance_id"] = json!("replacement");
            presence::seed_report(&conn, "client", replacement, now, None, false, "unknown");
        }
        assert_eq!(Enabled(PLUGIN).run(&event, &ctx).await, StepOutcome::Stop);
        db::set_setting(&database.lock().unwrap(), "plugin.notifier.enabled", "true").unwrap();
        assert_eq!(
            Enabled(PLUGIN).run(&event, &ctx).await,
            StepOutcome::Continue
        );
        event = persist(&database.lock().unwrap(), event);
        assert_eq!(Notify.run(&event, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            1,
            "old execution is no longer a recipient after re-enabling"
        );
        event.payload["task"]["owner_origin"]["instance_id"] = json!("replacement");
        event = persist(&database.lock().unwrap(), event);
        assert_eq!(Notify.run(&event, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            2,
            "replacement is visible without replaying presence notifications"
        );
        database
            .lock()
            .unwrap()
            .execute(
                "UPDATE task_target_presence SET report_seen_at=?1",
                params![now - presence::GONE_AFTER_MS],
            )
            .unwrap();
        event = persist(&database.lock().unwrap(), event);
        assert_eq!(Notify.run(&event, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            2,
            "expired reports are excluded before the next Presence sweep"
        );
        database
            .lock()
            .unwrap()
            .execute("DELETE FROM task_target_presence", [])
            .unwrap();
        event = persist(&database.lock().unwrap(), event);
        assert_eq!(Notify.run(&event, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            notices(),
            2,
            "departed sessions leave no notification projection"
        );
    }

    #[tokio::test]
    async fn notifications_and_receipt_commit_once_and_failed_batches_retry_atomically() {
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
        let event = {
            let conn = database.lock().unwrap();
            presence::seed_report(&conn, "client", agent(), now, None, false, "unknown");
            presence::seed_report(
                &conn,
                "client",
                json!({"session_id":"requester","instance_id":"peer","project":"p","generation":"g"}),
                now,
                None,
                false,
                "unknown",
            );
            conn.execute_batch(
                "CREATE TRIGGER fail_second_notification BEFORE INSERT ON router_events
                 WHEN NEW.id='agent.notify' AND json_extract(NEW.payload,'$.target')='requester'
                 BEGIN SELECT RAISE(ABORT,'notification storage unavailable'); END;",
            )
            .unwrap();
            persist(
                &conn,
                Event {
                    seq: 0,
                    id: "task.commented".into(),
                    version: 1,
                    uid: uuid::Uuid::nil(),
                    payload: payload(
                        json!({"owner_origin":{"session_id":ME,"instance_id":"i"},
                    "requester":{"origin":{"session_id":"requester","instance_id":"peer"},"generation":"g"}}),
                        Value::Null,
                    ),
                    source: "tasks".into(),
                    emitted_at: now,
                    correlation: None,
                },
            )
        };
        let counts = || {
            let conn = database.lock().unwrap();
            (
                conn.query_row(
                    "SELECT COUNT(*) FROM router_events WHERE id='agent.notify'",
                    [],
                    |row| row.get::<_, i64>(0),
                )
                .unwrap(),
                conn.query_row("SELECT COUNT(*) FROM notifier_processed", [], |row| {
                    row.get::<_, i64>(0)
                })
                .unwrap(),
            )
        };
        assert!(
            matches!(Notify.run(&event,&ctx).await,StepOutcome::Fail(error) if error.contains("notification storage unavailable"))
        );
        assert_eq!(
            counts(),
            (0, 0),
            "failed second recipient rolls back first notification and receipt"
        );
        database
            .lock()
            .unwrap()
            .execute_batch("DROP TRIGGER fail_second_notification")
            .unwrap();
        assert_eq!(Notify.run(&event, &ctx).await, StepOutcome::Continue);
        assert_eq!(counts(), (2, 1));
        initialize(&database.lock().unwrap()).unwrap();
        assert_eq!(Notify.run(&event, &ctx).await, StepOutcome::Continue);
        assert_eq!(
            counts(),
            (2, 1),
            "committed cause cannot notify twice after restart/replay"
        );
        database
            .lock()
            .unwrap()
            .execute("DELETE FROM router_events WHERE seq=?1", [event.seq])
            .unwrap();
        assert_eq!(
            counts(),
            (2, 0),
            "router retention also removes the plugin receipt"
        );
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
