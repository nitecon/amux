//! Exact-session task dispatch through the gateway's credential-bound API
//! (agent-gateway `docs/session-task-dispatch.md`). The amux wrapper session is
//! the declared execution session, the wrapper instance is the executor
//! generation, and the provisioned machine UUID is the authenticated instance.
//! Credentials are client-side hashes of secrets, not hardware or provider
//! attestation. A claim or an input is never an execution start.
use crate::identity::AgentIdentity;
use anyhow::{Context, Result, bail};
use reqwest::{Client, Method, Url};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    io::Write,
    path::{Path, PathBuf},
    time::Duration,
};
use uuid::Uuid;

const LIMIT: usize = 4 * 1024 * 1024;
const MAX_SESSIONS: usize = 1000;
const MAX_TITLE: usize = 512;
const MAX_SPECIFICATION: usize = 65_536;

/// The provisioned machine credential. It is issued by a gateway operator and
/// supplied by the user; amux never provisions or stores it.
pub struct Machine {
    pub instance_id: Uuid,
    pub key: String,
}

impl Machine {
    pub fn new(instance_id: Uuid, key: String) -> Result<Self> {
        if instance_id.is_nil() || !is_key(&key) {
            bail!("machine credential must be a UUID and a 64-character hex key");
        }
        Ok(Self { instance_id, key })
    }
}

/// One executor generation and its own session secret.
pub struct Executor {
    pub actor: AgentIdentity,
    session_key: String,
}

impl Executor {
    /// Loads this generation's secret, creating and persisting it first if needed.
    pub fn load(state: &Path, actor: AgentIdentity) -> Result<Self> {
        let path = key_path(state, &actor);
        let session_key = match std::fs::read_to_string(&path) {
            Ok(key) => key.trim().to_owned(),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                let mut bytes = [0u8; 32];
                getrandom::fill(&mut bytes).map_err(|e| anyhow::anyhow!("random source: {e}"))?;
                let key: String = bytes.iter().map(|b| format!("{b:02x}")).collect();
                write_private(&path, key.as_bytes())?;
                key
            }
            Err(e) => return Err(e.into()),
        };
        if !is_key(&session_key) {
            bail!("stored session credential is malformed; refusing to use it");
        }
        Ok(Self { actor, session_key })
    }

    pub fn generation(&self) -> Uuid {
        self.actor.instance_id
    }

    pub fn target(&self, machine: &Machine) -> Value {
        json!({
            "origin": {
                "session_id": self.actor.session_id.to_string(),
                "instance_id": machine.instance_id.to_string(),
                "provider": self.actor.provider.command(),
                "os": self.actor.os,
            },
            "generation": self.generation().to_string(),
        })
    }
}

fn is_key(key: &str) -> bool {
    key.len() == 64 && key.bytes().all(|b| b.is_ascii_hexdigit())
}

fn key_path(state: &Path, actor: &AgentIdentity) -> PathBuf {
    state
        .join("dispatch")
        .join("keys")
        .join(format!("{}-{}.key", actor.session_id, actor.instance_id))
}

fn write_private(path: &Path, bytes: &[u8]) -> Result<()> {
    std::fs::create_dir_all(path.parent().context("credential path has no parent")?)?;
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    Ok(())
}

/// A request persisted before it is sent, so a lost reply can be inspected and
/// replayed by the same execution with the same request ID.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Request {
    pub request_id: Uuid,
    pub project: String,
    pub session_id: Uuid,
    pub generation: Uuid,
    /// Path below `/v1/projects/{project}/`.
    pub path: Vec<String>,
    pub body: Value,
    /// `uncertain` until the gateway accepts or rejects it.
    pub status: String,
    pub response: Option<Value>,
    pub error: Option<String>,
}

fn request_path(state: &Path, id: Uuid) -> PathBuf {
    state
        .join("dispatch")
        .join("requests")
        .join(format!("{id}.json"))
}

fn save(state: &Path, request: &Request) -> Result<()> {
    let path = request_path(state, request.request_id);
    std::fs::create_dir_all(path.parent().context("request path has no parent")?)?;
    let temporary = path.with_extension("json.tmp");
    std::fs::write(&temporary, serde_json::to_vec_pretty(request)?)?;
    std::fs::rename(temporary, path)?;
    Ok(())
}

pub fn load_request(state: &Path, id: Uuid) -> Result<Request> {
    serde_json::from_slice(&std::fs::read(request_path(state, id)).context("unknown request ID")?)
        .context("stored request is malformed")
}

/// Requests whose outcome is not known, oldest first by file name.
pub fn uncertain(state: &Path) -> Result<Vec<Request>> {
    let directory = state.join("dispatch").join("requests");
    let entries = match std::fs::read_dir(&directory) {
        Ok(entries) => entries,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(e.into()),
    };
    let mut requests = Vec::new();
    for entry in entries {
        let path = entry?.path();
        if path.extension().is_some_and(|e| e == "json")
            && let Ok(request) = serde_json::from_slice::<Request>(&std::fs::read(&path)?)
            && request.status == "uncertain"
        {
            requests.push(request);
        }
    }
    requests.sort_by_key(|r| r.request_id);
    Ok(requests)
}

pub struct Dispatcher {
    client: Client,
    base: Url,
    token: String,
    project: String,
    machine: Machine,
    state: PathBuf,
}

/// Arguments for an action on an existing dispatch.
#[derive(Default)]
pub struct ActionArgs {
    pub claim_id: Option<String>,
    pub execution_id: Option<Uuid>,
    pub result: Option<String>,
    pub target: Option<Value>,
}

pub const OPERATIONS: [&str; 8] = [
    "receive",
    "claim",
    "start",
    "succeeded",
    "failed",
    "request_cancel",
    "cancel",
    "handoff",
];

impl Dispatcher {
    pub fn new(
        base: &str,
        token: String,
        project: &str,
        machine: Machine,
        state: PathBuf,
    ) -> Result<Self> {
        let base = Url::parse(base).context("invalid gateway URL")?;
        if !matches!(base.scheme(), "http" | "https")
            || base.host_str().is_none()
            || !base.username().is_empty()
            || base.password().is_some()
            || base.query().is_some()
            || base.fragment().is_some()
            || base.path() != "/"
        {
            bail!(
                "gateway URL must be an HTTP(S) origin without credentials, path, query or fragment"
            );
        }
        if project.is_empty()
            || project == "."
            || project == ".."
            || !project
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || "-_.".contains(c))
        {
            bail!("gateway project must be a project identifier");
        }
        Ok(Self {
            client: Client::builder()
                .timeout(Duration::from_secs(15))
                .redirect(reqwest::redirect::Policy::none())
                .build()?,
            base,
            token,
            project: project.to_owned(),
            machine,
            state,
        })
    }

    async fn send(
        &self,
        method: Method,
        path: &[String],
        body: Option<&Value>,
        executor: Option<&Executor>,
        query: &[(&str, String)],
    ) -> Result<Value> {
        let mut url = self.base.clone();
        url.path_segments_mut()
            .map_err(|_| anyhow::anyhow!("invalid gateway origin"))?
            .extend(["v1", "projects", &self.project])
            .extend(path);
        let mut request = self
            .client
            .request(method, url)
            .query(query)
            .bearer_auth(&self.token)
            .header("X-Gateway-Machine-Key", &self.machine.key)
            .header("X-Agent-Instance-Id", self.machine.instance_id.to_string());
        if let Some(executor) = executor {
            request = request
                .header("X-Agent-Session-Id", executor.actor.session_id.to_string())
                .header("X-Agent-Provider", executor.actor.provider.command())
                .header("X-Agent-OS", &executor.actor.os)
                .header("X-Gateway-Session-Key", &executor.session_key);
        }
        if let Some(body) = body {
            request = request.json(body);
        }
        let mut response = request
            .send()
            .await
            .context("gateway unavailable; outcome unknown")?;
        let status = response.status();
        if response.content_length().is_some_and(|n| n > LIMIT as u64) {
            bail!("gateway response exceeds 4 MiB; outcome unknown");
        }
        let mut bytes = Vec::new();
        while let Some(chunk) = response.chunk().await? {
            if bytes.len() + chunk.len() > LIMIT {
                bail!("gateway response exceeds 4 MiB; outcome unknown");
            }
            bytes.extend_from_slice(&chunk);
        }
        if status.is_client_error() {
            return Err(Rejected(
                status.as_u16(),
                String::from_utf8_lossy(&bytes).chars().take(500).collect(),
            )
            .into());
        }
        if !status.is_success() {
            bail!("gateway returned {status}; outcome unknown");
        }
        serde_json::from_slice(&bytes).context("gateway returned invalid JSON; outcome unknown")
    }

    /// Registers or heartbeats this exact generation.
    pub async fn register(&self, executor: &Executor) -> Result<Value> {
        let reply = self
            .send(
                Method::POST,
                &["task-sessions".into()],
                Some(&json!({"generation": executor.generation().to_string()})),
                Some(executor),
                &[],
            )
            .await?;
        if reply["target"] != executor.target(&self.machine)
            || reply["expires_at"].as_i64().is_none()
        {
            bail!("gateway did not acknowledge this exact registration");
        }
        Ok(reply)
    }

    /// Live eligible sessions in this project.
    pub async fn sessions(&self, executor: &Executor) -> Result<Value> {
        let reply = self
            .send(
                Method::GET,
                &["task-sessions".into()],
                None,
                Some(executor),
                &[],
            )
            .await?;
        let sessions = reply["sessions"]
            .as_array()
            .context("gateway session list is malformed")?;
        if sessions.len() > MAX_SESSIONS || sessions.iter().any(|s| !is_target(&s["target"])) {
            bail!("gateway session list is malformed or too large");
        }
        Ok(reply)
    }

    /// Read-only recovery for this exact generation.
    pub async fn recover(&self, executor: &Executor) -> Result<Value> {
        let reply = self
            .send(
                Method::GET,
                &["task-dispatches".into(), "recovery".into()],
                None,
                Some(executor),
                &[("generation", executor.generation().to_string())],
            )
            .await?;
        let me = executor.target(&self.machine);
        for key in ["pending", "machine_dispatches", "prior_claims", "requested"] {
            if !reply[key].is_array() {
                bail!("gateway recovery is missing {key}");
            }
        }
        if reply["pending"]
            .as_array()
            .unwrap()
            .iter()
            .any(|d| d["target"] != me)
            || reply["requested"]
                .as_array()
                .unwrap()
                .iter()
                .any(|d| d["requester"] != me)
        {
            bail!("gateway recovery contains work for another generation");
        }
        Ok(reply)
    }

    /// Creates exactly one dispatch for an exact target. Persisted before sending.
    #[allow(clippy::too_many_arguments)]
    pub async fn delegate(
        &self,
        executor: &Executor,
        target_project: &str,
        target: Value,
        title: &str,
        specification: &str,
        required_os: Option<&str>,
        required_provider: Option<&str>,
    ) -> Result<Value> {
        if !is_target(&target) {
            bail!("target needs origin session_id, instance_id, provider, os and a generation");
        }
        if title.trim().is_empty()
            || title.len() > MAX_TITLE
            || specification.len() > MAX_SPECIFICATION
        {
            bail!("title must be 1-512 bytes and specification at most 65,536 bytes");
        }
        let request_id = Uuid::new_v4();
        let body = json!({
            "request_id": request_id.to_string(),
            "generation": executor.generation().to_string(),
            "target_project_ident": target_project,
            "target": target,
            "title": title,
            "specification": specification,
            "required_os": required_os,
            "required_provider": required_provider,
        });
        self.submit(executor, request_id, vec!["task-dispatches".into()], body)
            .await
    }

    /// One explicit dispatch action. Persisted before sending.
    pub async fn act(
        &self,
        executor: &Executor,
        task_id: Uuid,
        operation: &str,
        args: ActionArgs,
    ) -> Result<Value> {
        if !OPERATIONS.contains(&operation) {
            bail!("operation must be one of {}", OPERATIONS.join(", "));
        }
        let needs_reason = matches!(
            operation,
            "succeeded" | "failed" | "request_cancel" | "cancel" | "handoff"
        );
        if needs_reason && args.result.as_deref().is_none_or(|r| r.trim().is_empty()) {
            bail!("{operation} requires a nonempty --result reason or outcome");
        }
        if matches!(operation, "start" | "succeeded" | "failed") && args.execution_id.is_none() {
            bail!("{operation} requires the executor's --execution-id");
        }
        if operation == "handoff" && args.target.as_ref().is_none_or(|t| !is_target(t)) {
            bail!("handoff requires a complete new target");
        }
        let request_id = Uuid::new_v4();
        let body = json!({
            "request_id": request_id.to_string(),
            "generation": executor.generation().to_string(),
            "operation": operation,
            "claim_id": args.claim_id,
            "execution_id": args.execution_id.map(|id| id.to_string()),
            "result": args.result,
            "target": args.target,
        });
        let path = vec![
            "task-dispatches".into(),
            task_id.to_string(),
            "actions".into(),
        ];
        self.submit(executor, request_id, path, body).await
    }

    /// Resends a persisted uncertain request, unchanged, from the execution that
    /// created it. A later generation never adopts an earlier one's request.
    pub async fn replay(&self, executor: &Executor, request_id: Uuid) -> Result<Value> {
        let request = load_request(&self.state, request_id)?;
        if request.status != "uncertain" {
            bail!("request {request_id} is already {}", request.status);
        }
        if request.project != self.project
            || request.session_id != executor.actor.session_id
            || request.generation != executor.generation()
        {
            bail!(
                "only the execution that created request {request_id} may replay it; use recovery to inspect its state"
            );
        }
        self.deliver(executor, request).await
    }

    async fn submit(
        &self,
        executor: &Executor,
        request_id: Uuid,
        path: Vec<String>,
        body: Value,
    ) -> Result<Value> {
        let request = Request {
            request_id,
            project: self.project.clone(),
            session_id: executor.actor.session_id,
            generation: executor.generation(),
            path,
            body,
            status: "uncertain".into(),
            response: None,
            error: None,
        };
        save(&self.state, &request).context("could not persist the request; nothing was sent")?;
        self.deliver(executor, request).await
    }

    async fn deliver(&self, executor: &Executor, mut request: Request) -> Result<Value> {
        let outcome = match self
            .send(
                Method::POST,
                &request.path,
                Some(&request.body),
                Some(executor),
                &[],
            )
            .await
        {
            Ok(reply) => self.check(executor, &request, reply),
            Err(e) => Err(e),
        };
        match &outcome {
            Ok(reply) => {
                request.status = "accepted".into();
                request.response = Some(reply.clone());
                request.error = None;
            }
            Err(e) if e.is::<Rejected>() => {
                request.status = "rejected".into();
                request.error = Some(format!("{e:#}"));
            }
            Err(e) => request.error = Some(format!("{e:#}")),
        }
        save(&self.state, &request)?;
        outcome.with_context(|| format!("request {}", request.request_id))
    }

    /// Accepts only an acknowledgment of exactly what was asked.
    fn check(&self, executor: &Executor, request: &Request, reply: Value) -> Result<Value> {
        let me = executor.target(&self.machine);
        let body = &request.body;
        let dispatch = &reply["dispatch"];
        let auth = &reply["authentication"];
        let task_id = dispatch["task_id"]
            .as_str()
            .and_then(|id| Uuid::parse_str(id).ok());
        if task_id.is_none()
            || reply["task"]["id"].as_str() != dispatch["task_id"].as_str()
            || auth["session"] != me["origin"]
            || auth["generation"] != me["generation"]
            || auth["machine"]["instance_id"] != me["origin"]["instance_id"]
            || !is_target(&dispatch["target"])
            || !is_target(&dispatch["requester"])
        {
            bail!(
                "gateway acknowledgment does not identify this exact execution and task; outcome unknown"
            );
        }
        let state = dispatch["state"].as_str().unwrap_or_default();
        let ok = match body["operation"].as_str() {
            None => {
                // Delegation.
                dispatch["project_ident"] == body["target_project_ident"]
                    && dispatch["source_project_ident"] == self.project
                    && dispatch["requester"] == me
                    && dispatch["target"] == body["target"]
                    && state == "pending"
            }
            Some(operation) => {
                let same_task = Some(request.path[1].as_str()) == dispatch["task_id"].as_str();
                let destination = dispatch["target"] == me;
                let party = destination || dispatch["requester"] == me;
                let claim = || dispatch["claim_id"] == body["claim_id"];
                let execution = || dispatch["execution_id"] == body["execution_id"];
                same_task
                    && match operation {
                        "receive" => destination && state == "received",
                        "claim" => {
                            destination
                                && state == "claimed"
                                && dispatch["claim_id"].as_str().is_some_and(|c| !c.is_empty())
                                && dispatch["started_at"].is_null()
                        }
                        "start" => destination && state == "running" && claim() && execution(),
                        "succeeded" | "failed" => {
                            destination && state == operation && claim() && execution()
                        }
                        "request_cancel" => party && dispatch["cancel_requested_at"].is_i64(),
                        "cancel" => party && state == "cancelled",
                        "handoff" => state == "pending" && dispatch["target"] == body["target"],
                        _ => false,
                    }
            }
        };
        if !ok {
            bail!(
                "gateway acknowledgment does not match the requested target, task, claim or state; outcome unknown"
            );
        }
        Ok(reply)
    }
}

fn is_target(target: &Value) -> bool {
    let uuid = |v: &Value| {
        v.as_str()
            .is_some_and(|s| Uuid::parse_str(s).is_ok_and(|u| !u.is_nil()))
    };
    let origin = &target["origin"];
    uuid(&origin["session_id"])
        && uuid(&origin["instance_id"])
        && matches!(origin["provider"].as_str(), Some("codex" | "claude"))
        && matches!(origin["os"].as_str(), Some("linux" | "windows" | "macos"))
        && uuid(&target["generation"])
}

/// A definite gateway rejection (4xx). The gateway records nothing for it.
#[derive(Debug)]
pub struct Rejected(pub u16, pub String);

impl std::fmt::Display for Rejected {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "gateway rejected the request ({}): {}", self.0, self.1)
    }
}

impl std::error::Error for Rejected {}

/// `amux dispatch` subcommands. Targets are the JSON objects printed by
/// `amux dispatch sessions`.
#[derive(clap::Subcommand)]
pub enum Command {
    /// Register or heartbeat this execution's generation (lease: 120 s).
    Register,
    /// List live eligible sessions in this project.
    Sessions,
    /// Read-only recovery: pending work, machine dispatches, prior claims, requests.
    Recover,
    /// List persisted requests whose outcome is unknown.
    Requests,
    /// Delegate one task to an exact registered target.
    Delegate {
        title: String,
        /// Target JSON: {"origin":{session_id,instance_id,provider,os},"generation"}.
        #[arg(long)]
        target: String,
        /// Destination project (defaults to this project).
        #[arg(long)]
        target_project: Option<String>,
        #[arg(long, default_value = "")]
        specification: String,
        #[arg(long)]
        require_os: Option<String>,
        #[arg(long)]
        require_provider: Option<String>,
    },
    /// Report one explicit action on a dispatch.
    Act {
        task: Uuid,
        #[arg(value_parser = OPERATIONS)]
        operation: String,
        #[arg(long)]
        claim_id: Option<String>,
        /// The executor's own execution UUID; required for start and outcomes.
        #[arg(long)]
        execution_id: Option<Uuid>,
        /// Outcome or reason; required for outcomes, cancellation and handoff.
        #[arg(long)]
        result: Option<String>,
        /// New target JSON for handoff.
        #[arg(long)]
        target: Option<String>,
    },
    /// Resend a persisted uncertain request unchanged (same execution only).
    Replay { request: Uuid },
}

fn parse_target(raw: &str) -> Result<Value> {
    let target: Value = serde_json::from_str(raw).context("target must be JSON")?;
    if !is_target(&target) {
        bail!("target needs origin session_id, instance_id, provider, os and a generation");
    }
    Ok(target)
}

pub async fn run(dispatcher: &Dispatcher, executor: &Executor, command: Command) -> Result<Value> {
    match command {
        Command::Register => dispatcher.register(executor).await,
        Command::Sessions => dispatcher.sessions(executor).await,
        Command::Recover => dispatcher.recover(executor).await,
        Command::Requests => Ok(serde_json::to_value(uncertain(&dispatcher.state)?)?),
        Command::Delegate {
            title,
            target,
            target_project,
            specification,
            require_os,
            require_provider,
        } => {
            let project = target_project.unwrap_or_else(|| dispatcher.project.clone());
            dispatcher
                .delegate(
                    executor,
                    &project,
                    parse_target(&target)?,
                    &title,
                    &specification,
                    require_os.as_deref(),
                    require_provider.as_deref(),
                )
                .await
        }
        Command::Act {
            task,
            operation,
            claim_id,
            execution_id,
            result,
            target,
        } => {
            let target = target.as_deref().map(parse_target).transpose()?;
            let args = ActionArgs {
                claim_id,
                execution_id,
                result,
                target,
            };
            dispatcher.act(executor, task, &operation, args).await
        }
        Command::Replay { request } => dispatcher.replay(executor, request).await,
    }
}
