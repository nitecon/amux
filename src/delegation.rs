//! Compact lifecycle delivery state. Task state and provider execution are never inferred here.
use crate::identity::Provider;
use anyhow::{Context, Result, bail, ensure};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::{
    collections::HashSet,
    io::{Read, Write},
    path::{Path, PathBuf},
};
use uuid::Uuid;

pub const MAX_PENDING: usize = 64;
pub const MAX_RECEIPTS: usize = 2048;
pub const MAX_EVENT_AGE_MS: i64 = 5 * 60 * 1000;
const MAX_JOURNAL: usize = 4 * 1024 * 1024;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Origin {
    pub session_id: Uuid,
    pub instance_id: Uuid,
    pub provider: Provider,
    pub os: String,
}
impl Origin {
    pub(crate) fn validate(&self) -> Result<()> {
        ensure!(
            !self.session_id.is_nil() && !self.instance_id.is_nil(),
            "Invalid event origin"
        );
        ensure!(
            matches!(self.os.as_str(), "linux" | "windows" | "macos"),
            "Invalid event OS"
        );
        Ok(())
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Kind {
    Created,
    Commented,
    Completed,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Event {
    pub id: i64,
    pub project: String,
    pub task: Uuid,
    pub kind: Kind,
    pub created_at: i64,
    pub origin: Option<Origin>,
    pub goal: Option<String>,
    #[serde(default)]
    pub routing: Box<crate::task_routing::Routing>,
    #[serde(default)]
    pub owner: Option<Origin>,
}
impl Event {
    /// Retain identifiers only. Task descriptions and comment bodies never reach input or the journal.
    pub fn parse(value: &Value) -> Result<Option<Self>> {
        let kind = match value["kind"].as_str() {
            Some("task_created" | "task_unclaimed") => Kind::Created,
            Some("task_commented") => Kind::Commented,
            Some("task_completed") => Kind::Completed,
            _ => bail!("Unsupported lifecycle event"),
        };
        if kind == Kind::Created
            && value["task"]["kind"] == "delegated"
            && value["task"]["delegated_to_task_id"].is_string()
        {
            return Ok(None);
        }
        if kind == Kind::Commented {
            identifier(
                value["comment"]["author"]
                    .as_str()
                    .context("Comment author missing")?,
            )?;
        }
        let task = value["task"]["id"].as_str().context("Task UUID missing")?;
        let task_id = Uuid::parse_str(task).context("Invalid task UUID")?;
        ensure!(
            task_id.to_string() == task && !task_id.is_nil(),
            "Invalid task UUID"
        );
        let origin = match value.get("origin") {
            None | Some(Value::Null) => None,
            Some(value) => {
                Some(serde_json::from_value(value.clone()).context("Invalid event origin")?)
            }
        };
        let goal = match value["task"].get("goal") {
            None | Some(Value::Null) => None,
            Some(Value::String(goal)) => normalize_goal(Some(goal))?,
            _ => bail!("Invalid task goal"),
        };
        let event = Self {
            id: value["id"].as_i64().context("Event ID missing")?,
            project: value["project_ident"]
                .as_str()
                .context("Event project missing")?
                .into(),
            task: task_id,
            kind,
            created_at: value["created_at"].as_i64().unwrap_or(0),
            origin,
            goal,
            routing: match value["task"].get("routing") {
                _ if value["kind"] == "task_unclaimed" => Default::default(),
                None | Some(Value::Null) => Default::default(),
                Some(value) => {
                    serde_json::from_value(value.clone()).context("Invalid task routing")?
                }
            },
            owner: match value["task"].get("owner_origin") {
                None | Some(Value::Null) => None,
                Some(value) => {
                    Some(serde_json::from_value(value.clone()).context("Invalid task owner")?)
                }
            },
        };
        event.validate()?;
        Ok(Some(event))
    }
    fn validate(&self) -> Result<()> {
        ensure!(
            self.id > 0 && !self.task.is_nil(),
            "Invalid lifecycle identity"
        );
        identifier(&self.project)?;
        if let Some(origin) = &self.origin {
            origin.validate()?;
        }
        if let Some(owner) = &self.owner {
            owner.validate()?;
        }
        if let crate::task_routing::Routing::Session { target } = self.routing.as_ref() {
            target.validate()?;
        }
        ensure!(
            normalize_goal(self.goal.as_deref())? == self.goal,
            "Invalid normalized goal"
        );
        Ok(())
    }
    pub fn fresh(&self, now_ms: i64) -> bool {
        self.created_at > 0
            && self.created_at <= now_ms.saturating_add(30_000)
            && now_ms.saturating_sub(self.created_at) <= MAX_EVENT_AGE_MS
    }
    pub fn assigned_terminal_text(&self, session: Uuid, name: &str) -> String {
        format!(
            "{} [Amux identity: {name}. Use amux --agent {session} tasks get {} and the same --agent ID for task mutations.]",
            self.terminal_text(),
            self.task
        )
    }

    pub fn terminal_text(&self) -> String {
        let title = match self.kind {
            Kind::Created => "New delegated task received",
            Kind::Commented => "New comment on task",
            Kind::Completed => "Task completed",
        };
        format!("<Gateway message>{title}: {}</Gateway message>", self.task)
    }
}

fn identifier(value: &str) -> Result<()> {
    ensure!(
        !value.trim().is_empty()
            && value.len() <= 512
            && !value.chars().any(char::is_control)
            && !value.contains(['<', '>']),
        "Invalid gateway identity"
    );
    Ok(())
}
pub fn normalize_goal(value: Option<&str>) -> Result<Option<String>> {
    let Some(value) = value else {
        return Ok(None);
    };
    ensure!(
        value.len() <= 128 && !value.chars().any(char::is_control),
        "Invalid task goal"
    );
    Ok((!value.trim().is_empty()).then(|| value.trim().to_owned()))
}

/// One arrival-time process attachment. Repository context is distinct from process identity.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Target {
    pub workspace: Uuid,
    pub terminal: u64,
    pub session: Uuid,
    pub pid: u32,
    pub generation: String,
    pub directory: PathBuf,
    pub project: String,
    pub origin: Option<Origin>,
    pub goal: Option<String>,
}
impl Target {
    /// Lifecycle token for this attachment; never changes the assigned agent identity.
    pub fn executor_generation(&self) -> Uuid {
        Uuid::new_v5(
            &self.session,
            format!("{}:{}", self.pid, self.generation).as_bytes(),
        )
    }
    fn validate(&self) -> Result<()> {
        ensure!(
            !self.workspace.is_nil()
                && !self.session.is_nil()
                && self.pid > 0
                && self.terminal != u64::MAX,
            "Invalid delivery target"
        );
        ensure!(
            !self.generation.is_empty()
                && self.generation.len() <= 256
                && self.directory.is_absolute(),
            "Invalid process attachment"
        );
        if !self.project.is_empty() {
            identifier(&self.project)?;
        }
        if let Some(origin) = &self.origin {
            origin.validate()?;
        }
        ensure!(
            normalize_goal(self.goal.as_deref())? == self.goal,
            "Invalid target goal"
        );
        ensure!(
            self.goal.is_none() || self.origin.is_some(),
            "A task goal requires verified actor context"
        );
        Ok(())
    }
    pub fn same_attachment(&self, other: &Self) -> bool {
        self.workspace == other.workspace
            && self.terminal == other.terminal
            && self.session == other.session
            && self.pid == other.pid
            && self.generation == other.generation
            && self.directory == other.directory
    }
    pub fn accepts(&self, event: &Event) -> bool {
        self.mismatch_reason(event).is_none()
    }
    pub fn mismatch_reason(&self, event: &Event) -> Option<&'static str> {
        use crate::task_routing::Routing;
        if self.project != event.project {
            Some("Agent repository does not match the task project")
        } else if event.goal.is_some() && self.goal != event.goal {
            Some("Task goal does not match this agent")
        } else if event.kind == Kind::Created && *event.routing == Routing::Unassigned {
            Some("Task queued without an agent notification")
        } else if event.kind == Kind::Created
            && let Routing::Session { target } = event.routing.as_ref()
            && (self.origin.as_ref() != Some(&target.origin)
                || self.executor_generation() != target.generation)
        {
            Some("Task targets another agent execution")
        } else if !(event.kind == Kind::Created && *event.routing == Routing::Broadcast)
            && event
                .owner
                .as_ref()
                .is_some_and(|owner| self.origin.as_ref() != Some(owner))
        {
            Some("Task belongs to another agent")
        } else if event.origin.as_ref().is_some_and(|origin| {
            self.origin.as_ref().is_some_and(|own| {
                own.session_id == origin.session_id && own.instance_id == origin.instance_id
            })
        }) {
            Some("This agent created the event; its own notification was suppressed")
        } else {
            None
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Outcome {
    Received,
    Queued,
    Submitting,
    Submitted,
    Skipped,
    Failed,
    Uncertain,
}
impl Outcome {
    pub fn terminal(self) -> bool {
        matches!(
            self,
            Self::Submitted | Self::Skipped | Self::Failed | Self::Uncertain
        )
    }
    pub fn wire(self) -> &'static str {
        match self {
            Self::Received => "received",
            Self::Queued | Self::Submitting => "queued",
            Self::Submitted => "injected",
            Self::Skipped => "skipped",
            Self::Failed => "failed",
            Self::Uncertain => "uncertain",
        }
    }
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Recipient {
    pub target: Target,
    pub outcome: Outcome,
    pub reason: String,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Receipt {
    pub event: Event,
    pub recipients: Vec<Recipient>,
    pub outcome: Outcome,
    pub reason: String,
    pub confirmed: bool,
}
impl Receipt {
    pub fn terminal(&self) -> bool {
        if self.recipients.is_empty() {
            self.outcome.terminal()
        } else {
            self.recipients.iter().all(|r| r.outcome.terminal())
        }
    }
    pub fn status(&self) -> &'static str {
        if self.recipients.is_empty() {
            return self.outcome.wire();
        }
        for outcome in [
            Outcome::Received,
            Outcome::Queued,
            Outcome::Submitting,
            Outcome::Uncertain,
            Outcome::Failed,
            Outcome::Submitted,
        ] {
            if self.recipients.iter().any(|r| r.outcome == outcome) {
                return outcome.wire();
            }
        }
        "skipped"
    }
    pub fn signature(&self) -> String {
        // The recorded frame has a status, not a request nonce: serialize one outstanding snapshot per event.
        format!(
            "{}/{}",
            self.status(),
            serde_json::to_string(
                &self
                    .recipients
                    .iter()
                    .map(|r| (
                        &r.target.session,
                        &r.target.workspace,
                        r.target.terminal,
                        r.outcome.wire()
                    ))
                    .collect::<Vec<_>>()
            )
            .expect("receipt identifiers serialize")
        )
    }
}

/// Private bounded journal. The consumer UUID is generated automatically, with no machine key.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Journal {
    pub version: u32,
    pub namespace: String,
    pub consumer: Uuid,
    pub cursor: Option<i64>,
    pub receipts: Vec<Receipt>,
    #[serde(default)]
    pub rejected: Vec<Rejected>,
}
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Rejected {
    pub id: i64,
    pub reason: String,
    pub confirmed: bool,
}
impl Journal {
    pub fn new(namespace: String) -> Self {
        Self {
            version: 1,
            namespace,
            consumer: Uuid::new_v4(),
            cursor: None,
            receipts: Vec::new(),
            rejected: Vec::new(),
        }
    }
    fn validate(&self) -> Result<()> {
        ensure!(
            self.version == 1 && !self.consumer.is_nil() && self.namespace.len() <= 1024,
            "Invalid delegation journal"
        );
        ensure!(
            self.cursor.is_none_or(|cursor| cursor >= 0),
            "Invalid delegation cursor"
        );
        ensure!(
            self.receipts.len() + self.rejected.len() <= MAX_RECEIPTS
                && self
                    .receipts
                    .iter()
                    .map(|r| r.recipients.len().max(1))
                    .sum::<usize>()
                    + self.rejected.len()
                    <= MAX_RECEIPTS,
            "Delegation receipt capacity reached"
        );
        let pending = self
            .receipts
            .iter()
            .map(|r| {
                if r.recipients.is_empty() {
                    usize::from(!r.outcome.terminal())
                } else {
                    r.recipients
                        .iter()
                        .filter(|p| !p.outcome.terminal())
                        .count()
                }
            })
            .sum::<usize>();
        ensure!(pending <= MAX_PENDING, "Delegation queue capacity reached");
        let mut events = HashSet::new();
        for receipt in &self.receipts {
            receipt.event.validate()?;
            ensure!(
                events.insert(receipt.event.id) && receipt.recipients.len() <= MAX_PENDING,
                "Invalid receipt group"
            );
            ensure!(receipt.reason.len() <= 512, "Invalid receipt reason");
            let mut targets = HashSet::new();
            for recipient in &receipt.recipients {
                recipient.target.validate()?;
                ensure!(
                    targets.insert(recipient.target.session) && recipient.reason.len() <= 512,
                    "Invalid recipient receipt"
                );
            }
        }
        for rejection in &self.rejected {
            ensure!(
                rejection.id > 0 && events.insert(rejection.id) && rejection.reason.len() <= 512,
                "Invalid rejected event fence"
            );
        }
        Ok(())
    }
    pub fn load(path: &Path, namespace: &str) -> Result<Self> {
        let file = match std::fs::File::open(path) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(Self::new(namespace.into()));
            }
            Err(error) => return Err(error.into()),
        };
        let mut bytes = Vec::new();
        file.take(MAX_JOURNAL as u64 + 1).read_to_end(&mut bytes)?;
        ensure!(
            bytes.len() <= MAX_JOURNAL,
            "Delegation journal exceeds four MiB"
        );
        let mut journal: Self = serde_json::from_slice(&bytes)
            .context("Invalid delegation journal; original preserved")?;
        journal.validate()?;
        if journal.namespace != namespace {
            return Ok(Self::new(namespace.into()));
        }
        // A fresh app may never replay historical input. An interrupted attempt is uncertain.
        for receipt in &mut journal.receipts {
            for recipient in &mut receipt.recipients {
                if !recipient.outcome.terminal() {
                    recipient.outcome = if recipient.outcome == Outcome::Submitting {
                        Outcome::Uncertain
                    } else {
                        Outcome::Skipped
                    };
                    recipient.reason =
                        "Application restarted; automatic input replay disabled".into();
                    receipt.confirmed = false;
                }
            }
            if !receipt.outcome.terminal() {
                receipt.outcome = if receipt
                    .recipients
                    .iter()
                    .any(|r| r.outcome == Outcome::Uncertain)
                {
                    Outcome::Uncertain
                } else {
                    Outcome::Skipped
                };
                receipt.reason = "Application restarted; automatic input replay disabled".into();
                receipt.confirmed = false;
            }
        }
        Ok(journal)
    }
    /// Persist the attempt before any terminal API call. Failure must pause delivery.
    pub fn save(&self, path: &Path) -> Result<()> {
        self.validate()?;
        let bytes = serde_json::to_vec(self)?;
        ensure!(
            bytes.len() <= MAX_JOURNAL,
            "Delegation journal exceeds four MiB"
        );
        let parent = path.parent().context("Delegation journal has no parent")?;
        std::fs::create_dir_all(parent)?;
        let mut file = tempfile::NamedTempFile::new_in(parent)?;
        file.write_all(&bytes)?;
        file.as_file().sync_all()?;
        file.persist(path).map_err(|e| e.error)?;
        #[cfg(unix)]
        std::fs::File::open(parent)?.sync_all()?;
        Ok(())
    }
}
