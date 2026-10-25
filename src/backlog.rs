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
    if crate::autoreview::held_finding(task) {
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
    Ok(format!(
        "{} Review this queued task against the current project state and its comments. Implement it if still viable; otherwise review it with the user, or close it with a reason if obsolete. Respect platform requirements. This task is already claimed by your amux identity.",
        event.assigned_terminal_text(target.session, name)
    ))
}

pub const REVIEW_COMMENT: &str = "Automatically assigned by amux after 30 seconds without output from this agent's terminal. Evaluate whether this backlog task is still required against the current project and its comments. If uncertain, ask the user. If no longer valid, close it with a reason; otherwise start execution. Respect the task's platform requirements.";

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
        let claimed = self
            .claim(&target.project, &task.to_string(), actor)
            .await?;
        // The durable claim receipt already fences this operation. A lost
        // acknowledgment must not cause a second claim or duplicate comment.
        self.request(
            reqwest::Method::POST,
            &target.project,
            &[&task.to_string(), "comments"],
            Some(json!({"content": REVIEW_COMMENT})),
            Some(actor),
        )
        .await?;
        Ok(Some(claimed))
    }
}
