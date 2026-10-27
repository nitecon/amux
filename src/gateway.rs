//! The gateway owns tasks. The client never manufactures successful ownership.
use crate::{gateway_project, identity::AgentIdentity};
use anyhow::{Context, Result, bail, ensure};
use reqwest::{Method, Url};
use serde_json::{Value, json};
use std::path::PathBuf;

#[derive(Clone)]
pub struct Gateway {
    base: Url,
    transport: crate::gateway_transport::TransportProfile,
}

/// Optional creation metadata shared by task creation and delegation.
#[derive(Default)]
pub struct TaskDetails {
    pub description: Option<String>,
    pub labels: Vec<String>,
    pub hostname: Option<String>,
}

impl TaskDetails {
    /// Merge into a creation body. Routing labels have dedicated flags, so a
    /// free-form label cannot bypass their acknowledgment checks.
    pub fn apply(&self, body: &mut Value) -> Result<()> {
        if self
            .description
            .as_ref()
            .is_some_and(|text| text.len() > 64 * 1024)
        {
            bail!("description exceeds 64 KiB; move long context to the specification");
        }
        for label in &self.labels {
            if label.trim().is_empty() || label.len() > 128 || label.chars().any(char::is_control) {
                bail!(
                    "labels must be non-empty, at most 128 bytes and contain no control characters"
                );
            }
            if label.starts_with("os:") || label == "autoreview-finding" {
                bail!("use --os or --review-finding instead of the `{label}` label");
            }
        }
        if self.hostname.as_ref().is_some_and(|host| {
            host.trim().is_empty() || host.len() > 255 || host.chars().any(char::is_control)
        }) {
            bail!(
                "hostname must be non-empty, at most 255 bytes and contain no control characters"
            );
        }
        if let Some(description) = &self.description {
            body["description"] = json!(description);
        }
        if !self.labels.is_empty() {
            let mut labels = body["labels"].as_array().cloned().unwrap_or_default();
            labels.extend(self.labels.iter().map(|label| json!(label)));
            body["labels"] = json!(labels);
        }
        if let Some(hostname) = &self.hostname {
            body["hostname"] = json!(hostname);
        }
        Ok(())
    }
}

impl Gateway {
    pub fn new(base: &str, token: String) -> Result<Self> {
        Self::from_profile(crate::gateway_transport::TransportProfile::legacy(
            base, token,
        )?)
    }

    pub fn from_profile(transport: crate::gateway_transport::TransportProfile) -> Result<Self> {
        let base = transport.snapshot()?.base;
        Ok(Self { base, transport })
    }

    pub fn transport_profile(&self) -> crate::gateway_transport::TransportProfile {
        self.transport.clone()
    }

    pub async fn request(
        &self,
        method: Method,
        project: &str,
        suffix: &[&str],
        body: Option<Value>,
        actor: Option<&AgentIdentity>,
    ) -> Result<Value> {
        let creating_task = method == Method::POST && suffix.is_empty();
        let task_resource = suffix.len() == 1 && (method == Method::GET || method == Method::PATCH);
        let posting_comment =
            method == Method::POST && suffix.len() == 2 && suffix[1] == "comments";
        let requested_status = if method == Method::PATCH {
            body.as_ref().and_then(|body| body.get("status")).cloned()
        } else {
            None
        };
        let path: Vec<_> = std::iter::once("tasks")
            .chain(suffix.iter().copied())
            .collect();
        let result = self
            .project_request(method, project, &path, body, actor, &[])
            .await?;
        if creating_task {
            if canonical_id(&result["id"]).is_none()
                || result["project_ident"] != project
                || !matches!(
                    result["status"].as_str(),
                    Some("todo" | "in_progress" | "done")
                )
            {
                bail!(
                    "gateway did not acknowledge a canonical task in this project; acceptance is uncertain, fetch tasks before retrying"
                );
            }
        } else if task_resource || posting_comment {
            let expected = uuid::Uuid::parse_str(suffix[0])
                .ok()
                .filter(|id| !id.is_nil());
            if posting_comment {
                if expected.is_none()
                    || canonical_id(&result["id"]).is_none()
                    || canonical_id(&result["task_id"]) != expected
                {
                    bail!(
                        "gateway did not acknowledge a canonical comment on the requested task; publication is uncertain, fetch the task before retrying"
                    );
                }
            } else if expected.is_none()
                || canonical_id(&result["id"]) != expected
                || result["project_ident"] != project
                || requested_status.is_some_and(|status| result["status"] != status)
            {
                bail!(
                    "gateway did not identify the requested task/project/state; operation is unconfirmed, fetch the task before continuing"
                );
            }
        }
        Ok(result)
    }

    pub(crate) async fn project_request(
        &self,
        method: Method,
        project: &str,
        suffix: &[&str],
        body: Option<Value>,
        actor: Option<&AgentIdentity>,
        query: &[(&str, String)],
    ) -> Result<Value> {
        validate_project(project)?;
        let mut url = self.base.clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|_| anyhow::anyhow!("invalid gateway origin"))?;
            path.extend(["v1", "projects", project]);
            path.extend(suffix);
        }
        self.exchange(method, url, body, actor, query).await
    }

    /// One bounded, origin-bound, non-redirecting JSON exchange.
    async fn exchange(
        &self,
        method: Method,
        url: Url,
        body: Option<Value>,
        actor: Option<&AgentIdentity>,
        query: &[(&str, String)],
    ) -> Result<Value> {
        let snapshot = self.transport.current().await?;
        let mut request = snapshot.request(method, url).query(query);
        if let Some(actor) = actor {
            request = request
                .header("X-Agent-Id", actor.gateway_id())
                .header("X-Agent-Session-Id", actor.session_id.to_string())
                .header("X-Agent-Instance-Id", actor.instance_id.to_string())
                .header("X-Agent-Provider", actor.provider.command())
                .header("X-Agent-OS", &actor.os);
        }
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .context("gateway unavailable; no acceptance or claim is established")?;
        let status = response.status();
        let length = response.content_length();
        if length.is_some_and(|n| n > 4 * 1024 * 1024) {
            bail!("gateway response exceeds 4 MiB");
        }
        let mut response = response;
        let mut bytes = Vec::new();
        while let Some(chunk) = response.chunk().await? {
            if bytes.len() + chunk.len() > 4 * 1024 * 1024 {
                bail!("gateway response exceeds 4 MiB");
            }
            bytes.extend_from_slice(&chunk);
        }
        if !status.is_success() {
            bail!("gateway returned {status}; operation not acknowledged");
        }
        if bytes.is_empty() {
            return Ok(Value::Null);
        }
        serde_json::from_slice(&bytes).context("gateway returned invalid JSON")
    }

    /// Resolve the gateway project for a workspace from its Git origin, as CMUX does:
    /// match canonical remotes, otherwise register the repository and match the reread.
    /// Existing mappings (aliases) are preserved; a new project is named by the
    /// lowercase repository basename, e.g. `github.com/org/amux` registers `amux`.
    /// A checkout with no origin resolves only through its explicit binding.
    pub async fn resolve_project(&self, directory: PathBuf) -> Result<String> {
        let lookup = directory.clone();
        let remote =
            tokio::task::spawn_blocking(move || gateway_project::configured_origin(&lookup))
                .await
                .context("workspace origin lookup failed")??;
        let Some(remote) = remote else {
            let root = tokio::task::spawn_blocking(move || {
                crate::project_identity::resolve(&directory).map(|identity| identity.directory)
            })
            .await
            .context("workspace root lookup failed")??;
            let project =
                gateway_project::bound_project_at(&crate::identity::state_directory()?, &root)?
                    .context(
                        "workspace has no Git origin; bind it to an ad-hoc gateway project with `amux tasks bind <project>`",
                    )?;
            ensure!(
                gateway_project::local_only(&project, &self.projects().await?)?,
                "bound gateway project is not an active ad-hoc project without a repository"
            );
            return Ok(project);
        };
        if let Some(project) = gateway_project::matching(&remote, &self.projects().await?)? {
            return Ok(project);
        }
        let repo_url = format!("https://{remote}");
        if gateway_project::repository(&repo_url).as_deref() != Some(remote.as_str()) {
            bail!("workspace origin cannot be registered as a gateway repository");
        }
        let ident = project_slug(&remote)?;
        let registered = self
            .exchange(
                Method::POST,
                self.projects_url()?,
                Some(json!({"ident": ident, "repo_url": repo_url})),
                None,
                &[],
            )
            .await
            .context("gateway project registration not confirmed")?;
        if registered["ident"]
            .as_str()
            .is_none_or(|ident| ident.trim().is_empty())
        {
            bail!("gateway did not acknowledge a registered project identity");
        }
        gateway_project::matching(&remote, &self.projects().await?)?.context(
            "gateway has no project mapped to this repository after registration; an existing project may hold its name",
        )
    }

    pub async fn projects(&self) -> Result<Value> {
        self.exchange(Method::GET, self.projects_url()?, None, None, &[])
            .await
    }

    /// User review clears a task's owner; its newest event snapshot with an
    /// owner still names the agent that last held it.
    pub async fn last_task_owner(
        &self,
        project: &str,
        task: uuid::Uuid,
    ) -> Result<Option<crate::delegation::Origin>> {
        validate_project(project)?;
        let events = self
            .exchange(
                Method::GET,
                self.base.join("v1/task-events")?,
                None,
                None,
                &[("project", project.into()), ("task_id", task.to_string())],
            )
            .await?;
        // Without a cursor the gateway returns the newest events first.
        Ok(events
            .as_array()
            .context("gateway task events are not a list")?
            .iter()
            .filter(|event| event["task"]["id"] == task.to_string())
            .find_map(|event| serde_json::from_value(event["task"]["owner_origin"].clone()).ok()))
    }

    /// Check before any mutation: old gateways must not silently drop subscriptions.
    pub async fn require_requester_subscriptions(&self) -> Result<()> {
        let value = self.exchange(
            Method::GET, self.base.join("v1/tasks/capabilities")?, None, None, &[],
        ).await.context("gateway does not confirm task requester subscriptions; no subscription mutation sent")?;
        ensure!(
            value["requester_subscriptions"].as_u64() == Some(1),
            "gateway does not support requester subscriptions version 1; no subscription mutation sent"
        );
        Ok(())
    }

    fn projects_url(&self) -> Result<Url> {
        let mut url = self.base.clone();
        url.path_segments_mut()
            .map_err(|_| anyhow::anyhow!("invalid gateway origin"))?
            .extend(["v1", "projects"]);
        Ok(url)
    }

    pub async fn list(&self, project: &str) -> Result<Value> {
        self.list_filtered(project, None, false).await
    }

    /// `status` is a comma-separated filter; without it the gateway lists
    /// todo and in_progress. `include_stale` adds done tasks past its falloff window.
    pub async fn list_filtered(
        &self,
        project: &str,
        status: Option<&str>,
        include_stale: bool,
    ) -> Result<Value> {
        let mut query = Vec::new();
        if let Some(status) = status {
            let statuses: Vec<_> = status.split(',').map(str::trim).collect();
            if statuses.iter().any(|status| {
                !matches!(
                    *status,
                    "todo" | "in_progress" | "user_review" | "done" | "cancelled"
                )
            }) {
                bail!(
                    "status filter accepts only todo, in_progress, user_review, done and cancelled"
                );
            }
            query.push(("status", statuses.join(",")));
        }
        if include_stale {
            query.push(("include_stale", "true".into()));
        }
        self.project_request(Method::GET, project, &["tasks"], None, None, &query)
            .await
    }

    /// Set a task's absolute rank; lower ranks sort first within a column.
    pub async fn rank(
        &self,
        project: &str,
        id: uuid::Uuid,
        rank: i64,
        actor: &AgentIdentity,
    ) -> Result<Value> {
        let result = self
            .request(
                Method::PATCH,
                project,
                &[&id.to_string()],
                Some(json!({"rank": rank})),
                Some(actor),
            )
            .await?;
        if result["rank"] != rank {
            bail!("gateway did not acknowledge rank {rank}; fetch the task before retrying");
        }
        Ok(result)
    }

    /// Include parked user decisions and completed work for the desktop lanes.
    pub async fn list_all(&self, project: &str) -> Result<Value> {
        let mut result = self
            .project_request(
                Method::GET,
                project,
                &["tasks"],
                None,
                None,
                &[(
                    "status",
                    "todo,in_progress,user_review,done,cancelled".into(),
                )],
            )
            .await?;
        // Lists contain summary rows without the detail endpoint's project_ident.
        // Bind only omitted fields to this authenticated, project-scoped request;
        // an explicit different project is still an invalid response.
        let tasks = result
            .as_array_mut()
            .context("Gateway task list is incomplete")?;
        for task in tasks {
            let row = task
                .as_object_mut()
                .context("Invalid gateway task summary")?;
            match row.get("project_ident") {
                None => {
                    row.insert("project_ident".into(), json!(project));
                }
                Some(value) if value == project => {}
                Some(_) => bail!("Gateway task summary belongs to a different project"),
            }
        }
        Ok(result)
    }

    /// Record cancellation without inferring that an executor has stopped.
    /// An uncertain response must be reconciled with `cancellation`, not retried.
    pub async fn cancel_task(
        &self,
        project: &str,
        id: uuid::Uuid,
        reason: &str,
        actor: &AgentIdentity,
    ) -> Result<Value> {
        let reason = reason.trim();
        if id.is_nil() || reason.is_empty() || reason.len() > 16_384 {
            bail!("cancellation requires a nonnil task UUID and a reason of 1..=16384 bytes");
        }
        let response = self
            .request(
                Method::POST,
                project,
                &[&id.to_string(), "cancellation"],
                Some(json!({"reason": reason})),
                Some(actor),
            )
            .await?;
        let record = &response["cancellation"];
        validate_cancellation(record, project, id)?;
        let origin = json!({
            "session_id": actor.session_id,
            "instance_id": actor.instance_id,
            "provider": actor.provider.command(),
            "os": actor.os,
        });
        let task = &response["task"];
        if record["reason"] != reason
            || record["reporter_origin"] != origin
            || (!record["prior_owner_origin"].is_null() && record["prior_owner_origin"] != origin)
            || canonical_id(&task["id"]) != Some(id)
            || task["project_ident"] != project
            || task["status"] != "cancelled"
            || task["owner_origin"] != record["prior_owner_origin"]
            || task["owner_agent_id"] != record["prior_owner_agent_id"]
            || task["started_at"] != record["prior_started_at"]
            || !task["done_at"].is_null()
        {
            bail!(
                "gateway cancellation acknowledgment is inconsistent; fetch its cancellation record before continuing"
            );
        }
        Ok(response)
    }

    /// Read the durable report after reconnect or an uncertain cancellation.
    pub async fn cancellation(&self, project: &str, id: uuid::Uuid) -> Result<Value> {
        if id.is_nil() {
            bail!("cancellation requires a nonnil task UUID");
        }
        let record = self
            .request(
                Method::GET,
                project,
                &[&id.to_string(), "cancellation"],
                None,
                None,
            )
            .await?;
        validate_cancellation(&record, project, id)?;
        Ok(record)
    }

    /// Inline scratch text uses the existing message threads, including resolved ones.
    pub async fn messages(&self, project: &str, limit: u16, offset: usize) -> Result<Value> {
        if !(1..=500).contains(&limit) {
            bail!("message limit must be between 1 and 500");
        }
        self.project_request(
            Method::GET,
            project,
            &["messages"],
            None,
            None,
            &[
                ("state", "all".into()),
                ("limit", limit.to_string()),
                ("offset", offset.to_string()),
            ],
        )
        .await
    }

    pub async fn message(&self, project: &str, id: u64) -> Result<Value> {
        if id == 0 || id > i64::MAX as u64 {
            bail!("message ID must be a positive signed 64-bit integer");
        }
        self.project_request(
            Method::GET,
            project,
            &["messages", &id.to_string(), "thread"],
            None,
            None,
            &[],
        )
        .await
    }

    /// No automatic retry: a lost response can follow a committed publication.
    pub async fn send_message(
        &self,
        project: &str,
        body: String,
        subject: Option<String>,
        actor: &AgentIdentity,
    ) -> Result<Value> {
        if body.trim().is_empty() {
            bail!("message body must not be empty");
        }
        // Axum's default JSON request limit is 2 MiB. Leave room for JSON escaping.
        let value = json!({"body":body,"subject":subject});
        if serde_json::to_vec(&value)?.len() > 1024 * 1024 {
            bail!("encoded message exceeds 1 MiB; use a smaller inline handoff");
        }
        let result = self
            .project_request(
                Method::POST,
                project,
                &["messages"],
                Some(value),
                Some(actor),
                &[],
            )
            .await?;
        if result["message_id"].as_i64().is_none_or(|id| id <= 0) {
            bail!(
                "gateway did not return a canonical message ID; publication is uncertain, check messages before retrying"
            );
        }
        Ok(result)
    }
    /// Existing cross-project delegation only; no machine or exact-session routing.
    pub async fn delegate(
        &self,
        project: &str,
        target_project: &str,
        title: String,
        specification: Option<String>,
        details: &TaskDetails,
        actor: &AgentIdentity,
    ) -> Result<Value> {
        self.delegate_with_requester(
            project,
            target_project,
            title,
            specification,
            details,
            actor,
            None,
        )
        .await
    }

    #[allow(clippy::too_many_arguments)] // Existing delegation arguments plus the explicit return subscription.
    pub async fn delegate_with_requester(
        &self,
        project: &str,
        target_project: &str,
        title: String,
        specification: Option<String>,
        details: &TaskDetails,
        actor: &AgentIdentity,
        requester: Option<&crate::task_routing::SessionTarget>,
    ) -> Result<Value> {
        validate_project(project)?;
        validate_project(target_project)?;
        if project.eq_ignore_ascii_case(target_project) {
            bail!(
                "delegation requires a different target project; exact-session assignment is not supported by this endpoint"
            );
        }
        let title = title.trim();
        if title.is_empty() || title.len() > 1024 || title.chars().any(char::is_control) {
            bail!(
                "delegation title must be non-empty, at most 1 KiB and contain no control characters"
            );
        }
        if specification
            .as_ref()
            .is_some_and(|text| text.len() > 64 * 1024)
        {
            bail!("delegation context exceeds 64 KiB; use a smaller inline handoff");
        }
        let mut body = json!({"target_project_ident":target_project,"title":title,"specification":specification});
        details.apply(&mut body)?;
        if let Some(requester) = requester {
            requester.validate()?;
            ensure!(
                requester.origin.session_id == actor.session_id
                    && requester.origin.instance_id == actor.instance_id
                    && requester.origin.provider == actor.provider
                    && requester.origin.os == actor.os,
                "requester must be the calling agent"
            );
            self.require_requester_subscriptions().await?;
            body["requester"] = json!(requester);
        }
        let result = self
            .project_request(
                Method::POST,
                project,
                &["tasks", "delegate"],
                Some(body),
                Some(actor),
                &[],
            )
            .await
            .context("delegation not confirmed; inspect tasks in both projects before retrying")?;
        let source = &result["source_task"];
        if let Some(requester) = requester {
            ensure!(
                source["requester"] == json!(requester),
                "delegation accepted without confirmed source requester; inspect both projects before retrying"
            );
        }
        let target = &result["target_task"];
        let link = &result["delegation"];
        let source_id = canonical_id(&source["id"]);
        let target_id = canonical_id(&target["id"]);
        if source_id.is_none()
            || target_id.is_none()
            || source_id == target_id
            || canonical_id(&link["id"]).is_none()
            || source["project_ident"] != project
            || target["project_ident"] != target_project
            || !matches!(
                source["status"].as_str(),
                Some("todo" | "in_progress" | "done")
            )
            || !matches!(
                target["status"].as_str(),
                Some("todo" | "in_progress" | "done")
            )
            || source["kind"] != "delegated"
            || source["delegated_to_project_ident"] != target_project
            || canonical_id(&source["delegated_to_task_id"]) != target_id
            || link["source_project_ident"] != project
            || link["target_project_ident"] != target_project
            || canonical_id(&link["source_task_id"]) != source_id
            || canonical_id(&link["target_task_id"]) != target_id
            || result["message_id"].as_i64().is_none_or(|id| id <= 0)
        {
            bail!(
                "gateway did not acknowledge canonical linked tasks in both requested projects; delegation is uncertain, inspect both projects before retrying"
            );
        }
        Ok(result)
    }

    pub async fn claim(&self, project: &str, task: &str, actor: &AgentIdentity) -> Result<Value> {
        let task_id = uuid::Uuid::parse_str(task).context("task ID must be a complete UUID")?;
        if task_id.is_nil() {
            bail!("task ID must not be nil");
        }
        let result = self
            .request(
                Method::PATCH,
                project,
                &[task],
                Some(json!({"status":"in_progress","owner_agent_id":if actor.agent_id.is_empty() {actor.session_id.to_string()} else {actor.display_name()}})),
                Some(actor),
            )
            .await?;
        // request() already bound the reply to this task, project and status.
        if result["status"] != "in_progress"
            || result["owner_origin"]["session_id"] != actor.session_id.to_string()
            || result["owner_origin"]["instance_id"] != actor.instance_id.to_string()
        {
            bail!(
                "gateway did not acknowledge this exact session/instance as owner of the requested task/project; fetch the task before continuing"
            );
        }
        Ok(result)
    }
}

fn validate_cancellation(record: &Value, project: &str, id: uuid::Uuid) -> Result<()> {
    let valid_origin = |origin: &Value| {
        canonical_id(&origin["session_id"]).is_some()
            && canonical_id(&origin["instance_id"]).is_some()
            && matches!(origin["provider"].as_str(), Some("codex" | "claude"))
            && matches!(origin["os"].as_str(), Some("linux" | "windows" | "macos"))
    };
    if canonical_id(&record["task_id"]) != Some(id)
        || record["project_ident"] != project
        || !record["reason"]
            .as_str()
            .is_some_and(|s| !s.trim().is_empty() && s.len() <= 16_384)
        || !matches!(
            record["prior_status"].as_str(),
            Some("todo" | "in_progress")
        )
        || !valid_origin(&record["reporter_origin"])
        || (!record["prior_owner_origin"].is_null() && !valid_origin(&record["prior_owner_origin"]))
        || ((record["prior_status"] == "in_progress" || !record["prior_owner_agent_id"].is_null())
            && record["prior_owner_origin"].is_null())
        || record["cancelled_at"].as_i64().is_none_or(|n| n <= 0)
        || record["executor_stop_confirmed"] != false
    {
        bail!(
            "gateway did not return a canonical cancellation report for this task/project; cancellation is unconfirmed"
        );
    }
    Ok(())
}

pub(crate) fn validate_project(project: &str) -> Result<()> {
    if project.is_empty()
        || project == "."
        || project == ".."
        || !project
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "-_.".contains(c))
    {
        bail!("gateway project must be a project identifier, not a URL or path");
    }
    Ok(())
}

/// Lowercase repository basename, matching `ProjectIdentity::gateway_project`.
fn project_slug(remote: &str) -> Result<String> {
    let slug = remote
        .rsplit('/')
        .next()
        .unwrap_or_default()
        .to_ascii_lowercase();
    validate_project(&slug).context("repository name is not a usable gateway project")?;
    Ok(slug)
}

fn canonical_id(value: &Value) -> Option<uuid::Uuid> {
    let text = value.as_str()?;
    let id = uuid::Uuid::parse_str(text).ok()?;
    (!id.is_nil() && id.to_string() == text).then_some(id)
}
