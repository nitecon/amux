//! Review: queues one independent review task for every completed task, using
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
