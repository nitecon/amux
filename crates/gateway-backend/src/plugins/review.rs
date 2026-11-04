//! Review: queues one independent review task for agent-completed tasks, using
//! the existing deduplicated `task_reviews` store (the same contract as
//! `POST /v1/projects/{ident}/tasks/{id}/review`).
use crate::{
    plugins::{tasks, Enabled},
    router::{Event, Plugin, Step, StepContext, StepOutcome, Subscription},
    task_dispatch::Conflict,
    task_events, task_reviews,
};
use async_trait::async_trait;

pub const PLUGIN: &str = "review";

pub struct Review;

impl Plugin for Review {
    fn id(&self) -> &'static str {
        PLUGIN
    }
    fn subscriptions(&self) -> Vec<Subscription> {
        vec![Subscription {
            event: "task.completed",
            versions: 1..=1,
            depends_on: Vec::new(),
        }]
    }
    fn pre_exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Enabled(PLUGIN))]
    }
    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Queue)]
    }
}

struct Queue;

#[async_trait]
impl Step for Queue {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        // Human closure is final. Only an attributed agent completion starts
        // automatic follow-up work; explicit review requests remain available.
        if event.payload["actor"].is_null() {
            return StepOutcome::Stop;
        }
        let (Some(project), Some(task)) = (
            event.payload["project"].as_str(),
            event.payload["task"]["id"].as_str(),
        ) else {
            return StepOutcome::Stop;
        };
        let result = {
            let conn = ctx.db().lock().unwrap();
            task_events::mutation(&conn, None, |tx| task_reviews::enqueue(tx, project, task))
        };
        tasks::wake();
        match result {
            Ok(_) => StepOutcome::Continue,
            // Reopened or deleted before this ran: nothing to review now.
            Err(error) if error.downcast_ref::<Conflict>().is_some() => StepOutcome::Stop,
            Err(error) => StepOutcome::Fail(error.to_string()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{db, router};
    use serde_json::{json, Value};
    use uuid::Uuid;

    fn status(value: &str) -> db::TaskUpdate<'_> {
        db::TaskUpdate {
            status: Some(value),
            goal: None,
            owner_agent_id: None,
            rank: None,
            title: None,
            description: None,
            details: None,
            labels: None,
            hostname: None,
        }
    }

    #[tokio::test]
    async fn human_closures_stay_completed_without_reviews_and_agent_completions_are_reviewed() {
        for (initial, human) in [
            ("todo", true),
            ("in_progress", true),
            ("user_review", true),
            ("in_progress", false),
        ] {
            let database = db::open(":memory:").unwrap();
            let agent = task_events::AgentOrigin {
                session_id: Uuid::now_v7().to_string(),
                instance_id: Uuid::now_v7().to_string(),
                provider: "codex".into(),
                os: "linux".into(),
            };
            let (id, payload) = {
                let conn = database.lock().unwrap();
                conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','none','',0)", []).unwrap();
                let source = task_events::mutation(&conn, Some(&agent), |conn| {
                    db::insert_task(conn, "p", "Source work", None, None, &[], None, "fixture")
                })
                .unwrap();
                if initial != "todo" {
                    for next in ["in_progress"]
                        .into_iter()
                        .chain((initial == "user_review").then_some("user_review"))
                    {
                        task_events::mutation(&conn, Some(&agent), |conn| {
                            db::update_task(
                                conn,
                                "p",
                                &source.id,
                                &status(next),
                                Some(&agent.session_id),
                            )
                        })
                        .unwrap();
                    }
                }
                if human {
                    task_events::mutation(&conn, None, |conn| {
                        db::update_task_from_user(conn, "p", &source.id, &status("done"))
                    })
                    .unwrap();
                } else {
                    task_events::mutation(&conn, Some(&agent), |conn| {
                        db::update_task(
                            conn,
                            "p",
                            &source.id,
                            &status("done"),
                            Some(&agent.session_id),
                        )
                    })
                    .unwrap();
                }
                let (task, actor): (String, Option<String>) = conn.query_row(
                    "SELECT task_json, actor_json FROM router_task_outbox WHERE kind='completed' AND json_extract(task_json,'$.id')=?1",
                    [&source.id], |row| Ok((row.get(0)?, row.get(1)?))
                ).unwrap();
                let payload = json!({
                    "project": "p", "task": serde_json::from_str::<Value>(&task).unwrap(),
                    "actor": actor.map(|value| serde_json::from_str::<Value>(&value).unwrap()),
                });
                (source.id, payload)
            };
            let (bus, worker) = router::start(database.clone(), tasks::events(), vec![]).unwrap();
            let event = Event {
                seq: 1,
                id: "task.completed".into(),
                version: 1,
                uid: Uuid::now_v7(),
                payload,
                source: "tasks".into(),
                emitted_at: db::now_ms(),
                correlation: None,
            };
            let ctx = router::test_context(&bus, PLUGIN);
            for _ in 0..2 {
                assert_eq!(
                    Queue.run(&event, &ctx).await,
                    if human {
                        StepOutcome::Stop
                    } else {
                        StepOutcome::Continue
                    }
                );
            }
            let conn = database.lock().unwrap();
            let source = db::get_task_detail(&conn, "p", &id).unwrap().unwrap().task;
            assert_eq!(source.status, "done");
            assert!(source.done_at.is_some());
            let reviews: i64 = conn
                .query_row(
                    "SELECT COUNT(*) FROM task_reviews WHERE source_task_id=?1",
                    [&id],
                    |row| row.get(0),
                )
                .unwrap();
            assert_eq!(reviews, if human { 0 } else { 1 });
            worker.abort();
        }
    }
}
