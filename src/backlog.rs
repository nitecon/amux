//! Current queue snapshots, distinct from replaying historical gateway events.
use crate::{
    delegation::{Event, Origin, Outcome, Target},
    gateway::Gateway,
    identity::AgentIdentity,
    task_routing::Routing,
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use uuid::Uuid;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Receipt {
    pub task: Uuid,
    pub revision: i64,
    pub target: Target,
    pub outcome: Outcome,
}

pub fn revision(task: &Value) -> i64 {
    task["updated_at"].as_i64().unwrap_or(0)
}

pub fn owns(task: &Value, target: &Target) -> bool {
    if !task["owner_origin"].is_null() {
        return serde_json::from_value::<Origin>(task["owner_origin"].clone())
            .is_ok_and(|owner| target.origin.as_ref() == Some(&owner));
    }
    task["owner_agent_id"].as_str().is_some_and(|owner| {
        target.agent_id.as_deref() == Some(owner)
            || target
                .origin
                .as_ref()
                .is_some_and(|origin| origin.session_id.to_string() == owner)
    })
}

pub fn eligible(task: &Value, target: &Target) -> bool {
    let excluded = format!("review-exclude:{}", target.session);
    if !crate::task_platform::matches(task, target.origin.as_ref().map(|o| o.os.as_str()))
        || task["labels"]
            .as_array()
            .is_some_and(|labels| labels.iter().any(|label| label == &excluded))
    {
        return false;
    }
    if task["id"]
        .as_str()
        .is_none_or(|id| Uuid::parse_str(id).is_err() || id == Uuid::nil().to_string())
        || task["project_ident"] != target.project
        || task["status"] != "todo"
        || (task["kind"] == "delegated" && task["delegated_to_task_id"].is_string())
        || ((!task["owner_origin"].is_null() || !task["owner_agent_id"].is_null())
            && !owns(task, target))
        || (!task["goal"].is_null() && task["goal"].as_str() != target.goal.as_deref())
    {
        return false;
    }
    let routing = if task["routing"].is_null() {
        Routing::Auto
    } else {
        let Ok(routing) = serde_json::from_value::<Routing>(task["routing"].clone()) else {
            return false;
        };
        routing
    };
    match routing {
        Routing::Session { target: session } => {
            target.origin.as_ref() == Some(&session.origin)
                && target.executor_generation() == session.generation
        }
        _ => true,
    }
}

pub fn review_text(task: &Value, target: &Target, name: &str, now: i64) -> Result<String> {
    let mut event = Event::parse(
        &json!({"id":1,"kind":"task_created", "project_ident":target.project,
        "created_at":now,"task":task}),
    )?
    .ok_or_else(|| anyhow::anyhow!("Not an executable task"))?;
    event.summary.as_mut().unwrap().heading = "Queued task review".into();
    if task["labels"]
        .as_array()
        .is_some_and(|labels| labels.iter().any(|label| label == "task-review"))
    {
        event.summary.as_mut().unwrap().heading = "Queued independent review".into();
        return Ok(format!(
            "{} This independent review task is already claimed by your amux identity. Review the source task, associated comments and actual changes according to this task's specification. Follow KISS/YAGNI. Review only: do not edit code or implement findings. Record confirmed defects as ordinary queued tasks, post results on the source and review tasks, then mark this review task done. Do not use the legacy reviews complete command.",
            event.assigned_terminal_text(target.session, name)
        ));
    }
    Ok(format!(
        "{} This task is already claimed by your amux identity. Check current code/comments and platform requirements; implement if needed, or close it with a reason if obsolete. If waiting for a user decision, comment with your question then run: amux --agent {} tasks user-review {}.",
        event.assigned_terminal_text(target.session, name),
        target.session,
        event.task
    ))
}

/// A one-time nudge for an idle agent that still owns in-progress work.
pub fn reminder_text(task: &Value, target: &Target, name: &str, now: i64) -> Result<String> {
    let mut event = Event::parse(
        &json!({"id":1,"kind":"task_created", "project_ident":target.project,
        "created_at":now,"task":task}),
    )?
    .ok_or_else(|| anyhow::anyhow!("Not an executable task"))?;
    event.summary.as_mut().unwrap().heading = "In-progress task reminder".into();
    Ok(format!(
        "{} You still own this task. Continue it, mark it done if complete, or release it if you cannot continue. If waiting for a user decision, comment with your question then run: amux --agent {} tasks user-review {}.",
        event.assigned_terminal_text(target.session, name),
        target.session,
        event.task
    ))
}

pub const REVIEW_COMMENT: &str = "Automatically assigned by amux after 30 seconds without output from this agent's terminal. Evaluate whether this task is still required. If a user decision is needed, record the question and move the task to User review. Close obsolete work with a reason; otherwise start execution. Respect platform requirements.";

impl Gateway {
    /// A fresh read prevents taking a changed/assigned item; the gateway arbitrates claims.
    pub async fn claim_backlog(
        &self,
        task: Uuid,
        expected_revision: i64,
        target: &Target,
        actor: &AgentIdentity,
    ) -> Result<Option<Value>> {
        target.validate()?;
        ensure!(
            target
                .origin
                .as_ref()
                .is_some_and(|origin| origin.session_id == actor.session_id
                    && origin.instance_id == actor.instance_id
                    && origin.provider == actor.provider
                    && origin.os == actor.os),
            "Backlog actor changed"
        );
        ensure!(
            self.resolve_project(target.directory.clone()).await? == target.project,
            "Repository project changed before backlog claim"
        );
        let current = self
            .request(
                reqwest::Method::GET,
                &target.project,
                &[&task.to_string()],
                None,
                None,
            )
            .await?;
        if revision(&current) != expected_revision || !eligible(&current, target) {
            return Ok(None);
        }
        let claimed = match self.claim(&target.project, &task.to_string(), actor).await {
            Ok(claimed) => claimed,
            // A definite rejection is contention, not a lost acknowledgment.
            // Refresh and pick the next eligible FIFO item; never pause an idle
            // agent indefinitely because a peer won the same head-of-queue race.
            Err(error)
                if error
                    .downcast_ref::<crate::gateway::GatewayStatus>()
                    .is_some_and(|status| {
                        matches!(
                            status.0,
                            reqwest::StatusCode::CONFLICT | reqwest::StatusCode::NOT_FOUND
                        )
                    }) =>
            {
                return Ok(None);
            }
            Err(error) => return Err(error),
        };
        // The durable claim receipt already fences this operation. A lost
        // acknowledgment must not cause a second claim or duplicate comment.
        if let Err(error) = self
            .request(
                reqwest::Method::POST,
                &target.project,
                &[&task.to_string(), "comments"],
                Some(json!({"content": REVIEW_COMMENT})),
                Some(actor),
            )
            .await
        {
            eprintln!("amux: queue claim confirmed; assignment comment unconfirmed: {error}");
        }
        Ok(Some(claimed))
    }
}
