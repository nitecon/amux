//! Human task actions from the desktop use bearer authentication, never an agent's identity.
use crate::{gateway::Gateway, task_routing::SessionTarget};
use anyhow::{Result, ensure};
use reqwest::Method;
use serde_json::{Value, json};
use uuid::Uuid;

#[derive(Debug, Clone)]
pub struct TaskEdits {
    pub title: String,
    pub description: String,
    pub specification: String,
}

impl TaskEdits {
    pub fn validate(&self) -> Result<()> {
        ensure!(!self.title.trim().is_empty(), "Enter a task title");
        ensure!(self.title.len() <= 1024, "Task title is too long");
        ensure!(
            self.description.len() <= 64 * 1024 && self.specification.len() <= 64 * 1024,
            "Task description or specification is too long"
        );
        Ok(())
    }
}

impl Gateway {
    pub async fn edit_desktop_task(
        &self,
        project: &str,
        task: Uuid,
        edits: &TaskEdits,
    ) -> Result<Value> {
        edits.validate()?;
        let value = self
            .request(
                Method::PATCH,
                project,
                &[&task.to_string()],
                Some(
                    json!({"title": edits.title, "description": edits.description,
                    "specification": edits.specification}),
                ),
                None,
            )
            .await?;
        ensure!(
            value["title"] == edits.title
                && value["description"] == edits.description
                && value["specification"]
                    .as_str()
                    .or(value["details"].as_str())
                    == Some(edits.specification.as_str()),
            "Task edits unconfirmed; refresh and inspect the task before retrying"
        );
        Ok(value)
    }

    pub async fn close_desktop_task(&self, project: &str, task: Uuid) -> Result<Value> {
        self.request(
            Method::PATCH,
            project,
            &[&task.to_string()],
            Some(json!({"status": "done"})),
            None,
        )
        .await
    }

    pub async fn comment_on_task(&self, project: &str, task: Uuid, content: &str) -> Result<Value> {
        ensure!(!content.trim().is_empty(), "Enter a comment");
        ensure!(content.len() <= 64 * 1024, "Comment is too long");
        let comment = self
            .request(
                Method::POST,
                project,
                &[&task.to_string(), "comments"],
                Some(json!({"content": content, "author_type": "user"})),
                None,
            )
            .await?;
        ensure!(
            comment["content"] == content && comment["author_type"] == "user",
            "Comment publication unconfirmed; refresh and inspect the thread before retrying"
        );
        Ok(comment)
    }

    pub async fn assign_desktop_task(
        &self,
        project: &str,
        task: Uuid,
        target: &SessionTarget,
    ) -> Result<Value> {
        target.validate()?;
        let value = self
            .request(
                Method::POST,
                project,
                &[&task.to_string(), "assign"],
                Some(json!({"target": target})),
                None,
            )
            .await?;
        ensure!(
            value["id"] == task.to_string()
                && value["project_ident"] == project
                && matches!(value["status"].as_str(), Some("todo" | "in_progress"))
                && serde_json::from_value::<crate::delegation::Origin>(
                    value["owner_origin"].clone()
                )
                .is_ok_and(|owner| owner == target.origin),
            "Task assignment unconfirmed; refresh and inspect the owner before retrying"
        );
        Ok(value)
    }
}
