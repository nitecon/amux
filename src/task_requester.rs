//! Exact requester metadata shared by lifecycle routing and explicit watcher APIs.
use crate::{
    delegation::{Event, Target},
    gateway::Gateway,
    task_routing::SessionTarget,
};
use anyhow::{Context, Result};
use serde_json::Value;

pub fn stored(task: &Value) -> Result<Option<SessionTarget>> {
    let Some(value) = task.get("requester").filter(|value| !value.is_null()) else {
        return Ok(None);
    };
    let target: SessionTarget =
        serde_json::from_value(value.clone()).context("Invalid task requester")?;
    target.validate()?;
    Ok(Some(target))
}

impl Gateway {
    /// Optional current-state check for raw task-stream consumers. The native
    /// event router selects audiences on the server; desktop delivery does not
    /// invoke this compatibility helper.
    pub async fn requester_is_current(&self, event: &Event, target: &Target) -> Result<bool> {
        if !target.requester_result(event) {
            return Ok(false);
        }
        let task = self
            .request(
                reqwest::Method::GET,
                &event.project,
                &[&event.task.to_string()],
                None,
                None,
            )
            .await?;
        Ok(stored(&task)?.as_ref() == event.requester.as_deref()
            && crate::task_platform::matches(&task, target.origin.as_ref().map(|o| o.os.as_str()))
            && task["goal"]
                .as_str()
                .is_none_or(|goal| target.goal.as_deref() == Some(goal)))
    }
}
