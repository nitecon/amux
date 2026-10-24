//! Ordinary desktop task routing uses the existing gateway bearer connection.
use crate::{delegation::Origin, gateway::Gateway, gateway_project, identity::AgentIdentity};
use anyhow::{Context, Result, ensure};
use reqwest::Method;
use serde::{Deserialize, Serialize};
use serde_json::json;
use uuid::Uuid;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SessionTarget {
    pub origin: Origin,
    pub generation: Uuid,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "mode", rename_all = "snake_case")]
pub enum Routing {
    #[default]
    Auto,
    Unassigned,
    Broadcast,
    Session {
        target: SessionTarget,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Available {
    pub target: SessionTarget,
    pub seen_at: i64,
    pub expires_at: i64,
    #[serde(default)]
    pub display_name: Option<String>,
    #[serde(default)]
    pub hostname: Option<String>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct Directory {
    pub targets: Vec<Available>,
    pub canonical_remote: String,
}

impl SessionTarget {
    pub fn validate(&self) -> Result<()> {
        self.origin.validate()?;
        ensure!(!self.generation.is_nil(), "Invalid executor generation");
        Ok(())
    }
}

impl Gateway {
    pub async fn create_routed_task(
        &self,
        project: &str,
        title: &str,
        context: &str,
        remote: Option<&str>,
        routing: &Routing,
    ) -> Result<Uuid> {
        let mut body = json!({"title": title, "specification": context});
        if let Some(remote) = remote {
            ensure!(
                gateway_project::repository(remote).is_some(),
                "Invalid task repository"
            );
            body["canonical_remote"] = json!(remote);
            ensure!(
                crate::gateway_project::matching(remote, &self.projects().await?)?.as_deref()
                    == Some(project),
                "Repository project changed; refresh before submitting"
            );
        }
        if *routing != Routing::Auto {
            remote.context("Repository required for explicit task routing")?;
            if let Routing::Session { target } = routing {
                target.validate()?;
            }
            body["routing"] = serde_json::to_value(routing)?;
        }
        // A user selecting a recipient never impersonates that agent.
        let task = self
            .request(Method::POST, project, &[], Some(body), None)
            .await?;
        if *routing != Routing::Auto {
            let acknowledged: Routing = serde_json::from_value(task["routing"].clone()).context(
                "Task accepted without confirmed routing; inspect the task before retrying",
            )?;
            ensure!(
                &acknowledged == routing,
                "Task routing unconfirmed; inspect the task before retrying"
            );
            if let Routing::Session { target } = routing {
                let owner: Origin = serde_json::from_value(task["owner_origin"].clone())
                    .context("Task recipient unconfirmed; inspect the task before retrying")?;
                ensure!(
                    owner == target.origin,
                    "Task recipient unconfirmed; inspect the task before retrying"
                );
            } else {
                ensure!(
                    task["owner_origin"].is_null(),
                    "Task unexpectedly assigned; inspect it before retrying"
                );
            }
        }
        serde_json::from_value(task["id"].clone()).map_err(Into::into)
    }

    pub async fn task_targets(&self, project: &str, remote: &str) -> Result<Directory> {
        let value = self
            .project_request(Method::GET, project, &["task-targets"], None, None, &[])
            .await?;
        let directory: Directory = serde_json::from_value(value)
            .context("Gateway does not provide a valid task recipient directory")?;
        ensure!(
            gateway_project::repository(&directory.canonical_remote).as_deref() == Some(remote),
            "Task recipients belong to a different repository"
        );
        ensure!(directory.targets.len() <= 4096, "Too many task recipients");
        for entry in &directory.targets {
            entry.target.validate()?;
            ensure!(
                entry.seen_at > 0
                    && entry.expires_at > entry.seen_at
                    && entry.expires_at.saturating_sub(entry.seen_at) <= 120_000,
                "Invalid recipient availability"
            );
        }
        Ok(directory)
    }

    pub async fn advertise_task_target(
        &self,
        project: &str,
        remote: &str,
        actor: &AgentIdentity,
        generation: Uuid,
    ) -> Result<()> {
        let bare = json!({"generation": generation, "canonical_remote": remote});
        let mut labeled = bare.clone();
        labeled["display_name"] = json!(actor.display_name());
        labeled["hostname"] = json!(sysinfo::System::host_name());
        let response = self
            .project_request(
                Method::POST,
                project,
                &["task-targets"],
                Some(labeled),
                Some(actor),
                &[],
            )
            .await;
        // Old gateways reject unknown label fields. Presence is an idempotent
        // heartbeat, so only that request may fall back to its original shape.
        let value = match response {
            Err(error) if error.to_string().contains("400 Bad Request") => {
                self.project_request(
                    Method::POST,
                    project,
                    &["task-targets"],
                    Some(bare),
                    Some(actor),
                    &[],
                )
                .await?
            }
            response => response?,
        };
        let advertised: Available = serde_json::from_value(value.clone())?;
        ensure!(
            advertised.target.origin.session_id == actor.session_id
                && advertised.target.origin.instance_id == actor.instance_id
                && advertised.target.origin.provider == actor.provider
                && advertised.target.origin.os == actor.os
                && advertised.target.generation == generation
                && value["canonical_remote"]
                    .as_str()
                    .and_then(gateway_project::repository)
                    .as_deref()
                    == Some(remote),
            "Gateway did not acknowledge this agent in this repository"
        );
        Ok(())
    }
}
