//! The gateway owns tasks. The client never manufactures successful ownership.
use crate::identity::AgentIdentity;
use anyhow::{Context, Result, bail};
use reqwest::{Client, Method, Url};
use serde_json::{Value, json};
use std::time::Duration;

#[derive(Clone)]
pub struct Gateway {
    client: Client,
    base: Url,
    token: String,
}

impl Gateway {
    pub fn new(base: &str, token: String) -> Result<Self> {
        let base = Url::parse(base).context("invalid gateway URL")?;
        if !matches!(base.scheme(), "http" | "https")
            || base.host_str().is_none()
            || !base.username().is_empty()
            || base.password().is_some()
            || base.query().is_some()
            || base.fragment().is_some()
        {
            bail!("gateway URL must be HTTP(S), without credentials, query or fragment");
        }
        if base.path() != "/" {
            bail!("gateway URL must be an origin, without a path prefix");
        }
        Ok(Self {
            client: Client::builder()
                .timeout(Duration::from_secs(15))
                .redirect(reqwest::redirect::Policy::none())
                .build()?,
            base,
            token,
        })
    }

    pub async fn request(
        &self,
        method: Method,
        project: &str,
        suffix: &[&str],
        body: Option<Value>,
        actor: Option<&AgentIdentity>,
    ) -> Result<Value> {
        let path: Vec<_> = std::iter::once("tasks")
            .chain(suffix.iter().copied())
            .collect();
        self.project_request(method, project, &path, body, actor, &[])
            .await
    }

    async fn project_request(
        &self,
        method: Method,
        project: &str,
        suffix: &[&str],
        body: Option<Value>,
        actor: Option<&AgentIdentity>,
        query: &[(&str, String)],
    ) -> Result<Value> {
        if project.is_empty()
            || project == "."
            || project == ".."
            || !project
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || "-_.".contains(c))
        {
            bail!("gateway project must be a project identifier, not a URL or path");
        }
        let mut url = self.base.clone();
        {
            let mut path = url
                .path_segments_mut()
                .map_err(|_| anyhow::anyhow!("invalid gateway origin"))?;
            path.extend(["v1", "projects", project]);
            path.extend(suffix);
        }
        let mut request = self
            .client
            .request(method, url)
            .query(query)
            .bearer_auth(&self.token);
        if let Some(actor) = actor {
            request = request
                .header("X-Agent-Id", actor.session_id.to_string())
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

    pub async fn list(&self, project: &str) -> Result<Value> {
        self.request(Method::GET, project, &[], None, None).await
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
    pub async fn claim(&self, project: &str, task: &str, actor: &AgentIdentity) -> Result<Value> {
        uuid::Uuid::parse_str(task).context("task ID must be a complete UUID")?;
        let result = self
            .request(
                Method::PATCH,
                project,
                &[task],
                Some(json!({"status":"in_progress","owner_agent_id":actor.session_id.to_string()})),
                Some(actor),
            )
            .await?;
        if result["status"] != "in_progress"
            || result["owner_origin"]["session_id"] != actor.session_id.to_string()
            || result["owner_origin"]["instance_id"] != actor.instance_id.to_string()
        {
            bail!(
                "gateway did not acknowledge this exact session/instance as owner; fetch the task before continuing"
            );
        }
        Ok(result)
    }
}
