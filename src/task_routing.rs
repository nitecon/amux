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

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Directory {
    pub targets: Vec<Available>,
    /// `None` only for an explicitly bound ad-hoc project without a repository.
    pub canonical_remote: Option<String>,
}

impl SessionTarget {
    pub fn validate(&self) -> Result<()> {
        self.origin.validate()?;
        ensure!(!self.generation.is_nil(), "Invalid executor generation");
        Ok(())
    }
}

impl Gateway {
    /// Resolve the caller's exact advertised execution, never a display-name peer.
    pub async fn requester_target(
        &self,
        project: &str,
        actor: &AgentIdentity,
    ) -> Result<SessionTarget> {
        let directory = self
            .task_agents(project, &actor.project_directory, None)
            .await?;
        let origin = Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        };
        let mut matching = directory
            .targets
            .into_iter()
            .filter(|entry| entry.target.origin == origin);
        let target = matching
            .next()
            .context(
                "requesting agent is not currently advertised; refresh amux agent availability",
            )?
            .target;
        ensure!(
            matching.next().is_none(),
            "requesting agent has multiple executions; refresh availability"
        );
        Ok(target)
    }

    pub async fn create_task_with_requester(
        &self,
        project: &str,
        mut body: serde_json::Value,
        actor: &AgentIdentity,
        requester: &SessionTarget,
    ) -> Result<serde_json::Value> {
        requester.validate()?;
        ensure!(
            requester.origin
                == Origin {
                    session_id: actor.session_id,
                    instance_id: actor.instance_id,
                    provider: actor.provider,
                    os: actor.os.clone()
                },
            "requester must be the calling agent"
        );
        self.require_requester_subscriptions().await?;
        body["requester"] = json!(requester);
        let task = self
            .request(Method::POST, project, &[], Some(body), Some(actor))
            .await?;
        ensure!(
            task.get("requester")
                .and_then(|v| serde_json::from_value::<SessionTarget>(v.clone()).ok())
                .as_ref()
                == Some(requester),
            "task accepted without confirmed requester; inspect the task before retrying"
        );
        Ok(task)
    }

    /// Human callers pass no actor; agent callers can only manage an authorized self subscription.
    pub async fn set_task_requester(
        &self,
        project: &str,
        task: Uuid,
        requester: Option<&SessionTarget>,
        actor: Option<&AgentIdentity>,
    ) -> Result<serde_json::Value> {
        if let Some(target) = requester {
            target.validate()?;
        }
        self.require_requester_subscriptions().await?;
        let result = self
            .project_request(
                Method::PUT,
                project,
                &["tasks", &task.to_string(), "requester"],
                Some(json!({"requester":requester})),
                actor,
                &[],
            )
            .await?;
        let acknowledged = result
            .get("requester")
            .context("requester acknowledgment missing; inspect task before retrying")?;
        let expected = json!(requester);
        ensure!(
            result["id"] == task.to_string()
                && result["project_ident"] == project
                && *acknowledged == expected,
            "requester not confirmed for this task; inspect task before retrying"
        );
        Ok(result)
    }

    /// Fresh, repository-scoped discovery; this does not assign or claim work.
    pub async fn task_agents(
        &self,
        project: &str,
        directory: &std::path::Path,
        os: Option<crate::task_platform::TaskOs>,
    ) -> Result<Directory> {
        let root = crate::project_identity::resolve(directory)?;
        ensure!(
            self.resolve_project(root.directory).await? == project,
            "Workspace project changed"
        );
        let mut agents = self.task_targets(project, root.upstream.as_deref()).await?;
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)?
            .as_millis() as i64;
        agents.targets.retain(|entry| {
            entry.expires_at > now
                && crate::task_platform::compatible(os, Some(&entry.target.origin.os))
        });
        Ok(agents)
    }

    pub async fn reassign_agent_task(
        &self,
        project: &str,
        task: Uuid,
        session: Uuid,
        actor: &AgentIdentity,
    ) -> Result<serde_json::Value> {
        let current = self
            .request(Method::GET, project, &[&task.to_string()], None, None)
            .await?;
        ensure!(
            matches!(current["status"].as_str(), Some("todo" | "in_progress")),
            "Only active tasks can be reassigned"
        );
        let required = crate::task_platform::required_os(&current)?;
        let agents = self
            .task_agents(project, &actor.project_directory, required)
            .await?;
        let matching: Vec<_> = agents
            .targets
            .iter()
            .filter(|entry| entry.target.origin.session_id == session)
            .collect();
        ensure!(
            matching.len() == 1,
            "Recipient is unavailable, incompatible or has multiple live executions; refresh agents"
        );
        let target = &matching[0].target;
        let value = self
            .request(
                Method::POST,
                project,
                &[&task.to_string(), "reassign"],
                Some(json!({"target":target})),
                Some(actor),
            )
            .await?;
        ensure!(
            value["id"] == task.to_string()
                && value["project_ident"] == project
                && value["status"] == "todo"
                && serde_json::from_value::<Origin>(value["owner_origin"].clone())
                    .is_ok_and(|o| o == target.origin)
                && crate::task_platform::required_os(&value)? == required,
            "Reassignment unconfirmed; inspect the task before retrying"
        );
        let routing: Routing = serde_json::from_value(value["routing"].clone())
            .context("Reassignment routing unconfirmed; inspect before retrying")?;
        if let Routing::Session {
            target: acknowledged,
        } = routing
        {
            ensure!(
                acknowledged == *target,
                "Reassignment retained another execution's routing; inspect before continuing"
            );
        }
        Ok(value)
    }

    /// Retag only this actor's active work. The gateway atomically releases an
    /// incompatible owner; the client never emulates that with a second PATCH.
    pub async fn set_task_os(
        &self,
        project: &str,
        task: Uuid,
        os: Option<crate::task_platform::TaskOs>,
        actor: &AgentIdentity,
    ) -> Result<serde_json::Value> {
        ensure!(
            self.resolve_project(actor.project_directory.clone())
                .await?
                == project,
            "Workspace project changed"
        );
        let current = self
            .request(Method::GET, project, &[&task.to_string()], None, None)
            .await?;
        let origin = Origin {
            session_id: actor.session_id,
            instance_id: actor.instance_id,
            provider: actor.provider,
            os: actor.os.clone(),
        };
        ensure!(
            matches!(current["status"].as_str(), Some("todo" | "in_progress"))
                && serde_json::from_value::<Origin>(current["owner_origin"].clone())
                    .is_ok_and(|o| o == origin),
            "Only the exact owning agent may change this task's OS requirement"
        );
        let labels = crate::task_platform::labels_with_os(&current, os)?;
        let value = self
            .request(
                Method::PATCH,
                project,
                &[&task.to_string()],
                Some(json!({"labels":labels})),
                Some(actor),
            )
            .await?;
        ensure!(
            value["labels"] == json!(labels) && crate::task_platform::required_os(&value)? == os,
            "OS requirement unconfirmed; inspect before retrying"
        );
        if !crate::task_platform::compatible(os, Some(&actor.os)) {
            ensure!(
                value["status"] == "todo"
                    && ["owner_agent_id", "owner_origin", "started_at", "done_at"]
                        .iter()
                        .all(|key| value.get(*key).is_some_and(serde_json::Value::is_null))
                    && !matches!(
                        serde_json::from_value::<Routing>(value["routing"].clone()),
                        Ok(Routing::Session { .. })
                    ),
                "OS handoff unconfirmed; gateway must release the incompatible owner and route atomically. Inspect before retrying"
            );
        }
        Ok(value)
    }

    pub async fn create_routed_task(
        &self,
        project: &str,
        title: &str,
        context: &str,
        remote: Option<&str>,
        routing: &Routing,
    ) -> Result<Uuid> {
        let mut body = json!({"title": title, "specification": context,
            "hostname": sysinfo::System::host_name()});
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
            if remote.is_none() {
                ensure!(
                    gateway_project::local_only(project, &self.projects().await?)?,
                    "Repository required for explicit task routing"
                );
            }
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

    /// `remote` is `None` only for a local-only checkout bound to an ad-hoc project.
    pub async fn task_targets(&self, project: &str, remote: Option<&str>) -> Result<Directory> {
        let value = self
            .project_request(Method::GET, project, &["task-targets"], None, None, &[])
            .await?;
        let directory: Directory = serde_json::from_value(value)
            .context("Gateway does not provide a valid task recipient directory")?;
        ensure!(
            directory
                .canonical_remote
                .as_deref()
                .map(gateway_project::repository)
                == remote.map(|remote| Some(remote.to_string())),
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
        remote: Option<&str>,
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
                && match remote {
                    Some(remote) => {
                        value["canonical_remote"]
                            .as_str()
                            .and_then(gateway_project::repository)
                            .as_deref()
                            == Some(remote)
                    }
                    None => value["canonical_remote"].is_null(),
                },
            "Gateway did not acknowledge this agent in this repository"
        );
        Ok(())
    }
}
