//! Consented desktop recovery transport. This module never claims or starts work.
use amux::{
    dispatch::{ActionArgs, Dispatcher, Executor, Machine, Request},
    identity::{self, AgentIdentity},
};
use anyhow::{Context, Result, bail};
use serde::Deserialize;
use serde_json::Value;
use std::{
    collections::{HashMap, HashSet},
    io::Read,
    path::{Path, PathBuf},
    time::{Instant, SystemTime, UNIX_EPOCH},
};
use uuid::Uuid;

const MAX_LIST: usize = 4096;
const MAX_WORK: usize = 128;

/// Secrets are private, and deliberately have no Debug implementation.
#[derive(Clone)]
pub struct Credentials {
    endpoint: String,
    project: String,
    token: String,
    machine_id: Uuid,
    machine_key: String,
    state: PathBuf,
}

impl Credentials {
    pub fn from_preferences(
        project: &str,
        settings: &amux::gateway_preferences::GatewayPreferences,
    ) -> Result<Self> {
        if !is_project(project) {
            bail!("recovery requires a canonical project identifier");
        }
        settings.gateway()?;
        // Journal entries use Url's canonical origin, including its trailing slash.
        let endpoint = reqwest::Url::parse(settings.url.trim())?.to_string();
        let token = settings.api_key.clone();
        let machine_id = canonical_uuid(&settings.machine_id)
            .context("Set provisioned machine credentials in Gateway preferences for recovery")?;
        let machine_key = settings.machine_key.clone();
        Machine::new(machine_id, machine_key.clone())
            .map_err(|_| anyhow::anyhow!("invalid recovery machine credential"))?;
        Ok(Self {
            endpoint,
            project: project.into(),
            token,
            machine_id,
            machine_key,
            state: identity::state_directory()
                .map_err(|_| anyhow::anyhow!("recovery state directory unavailable"))?,
        })
    }

    fn open(&self, actor: AgentIdentity) -> Result<(Dispatcher, Executor, Target)> {
        if self.machine_id == actor.instance_id {
            bail!("provisioned machine UUID must differ from executor generation");
        }
        let machine = Machine::new(self.machine_id, self.machine_key.clone())?;
        let me = Target {
            origin: Origin {
                session_id: actor.session_id.to_string(),
                instance_id: self.machine_id.to_string(),
                provider: actor.provider.command().into(),
                os: actor.os.clone(),
            },
            generation: actor.instance_id.to_string(),
        };
        me.validate()?;
        let executor = Executor::load(&self.state, actor)
            .map_err(|_| anyhow::anyhow!("recovery executor credential unavailable"))?;
        let dispatcher = Dispatcher::new(
            &self.endpoint,
            self.token.clone(),
            &self.project,
            machine,
            self.state.clone(),
        )
        .map_err(|_| anyhow::anyhow!("recovery transport or journal unavailable"))?;
        Ok((dispatcher, executor, me))
    }
}

#[derive(Clone, Debug)]
pub struct Work {
    pub task: Uuid,
    pub text: String,
}

#[derive(Clone, Debug)]
pub struct Recovery {
    pub ready: Vec<Work>,
    pub awaiting_receipt: Vec<Work>,
    pub unresolved_receipts: HashSet<Uuid>,
    pub observed_at_ms: u64,
    pub queried_at: Instant,
}

pub async fn query(credentials: Credentials, actor: AgentIdentity) -> Result<Recovery> {
    let queried_at = Instant::now();
    let (dispatcher, executor, me) = credentials.open(actor)?;
    // Discard transport error bodies: a gateway may include arbitrary task text.
    dispatcher
        .register(&executor)
        .await
        .map_err(|_| anyhow::anyhow!("recovery registration deferred"))?;
    let directory = dispatcher
        .sessions(&executor)
        .await
        .map_err(|_| anyhow::anyhow!("recovery directory unavailable"))?;
    let snapshot = dispatcher
        .recover(&executor)
        .await
        .map_err(|_| anyhow::anyhow!("recovery snapshot unavailable"))?;
    let uncertain = dispatcher
        .uncertain()
        .map_err(|_| anyhow::anyhow!("recovery journal unavailable"))?;
    parse(
        &credentials,
        &me,
        directory,
        snapshot,
        &uncertain,
        queried_at,
        &executor.actor.project_directory,
    )
}

/// A single new receipt intent, never a claim/start or an automatic replay.
pub async fn receive(credentials: Credentials, actor: AgentIdentity, task: Uuid) -> Result<()> {
    if task.is_nil() {
        bail!("recovery task UUID must not be nil");
    }
    let (dispatcher, executor, me) = credentials.open(actor)?;
    let uncertain = dispatcher
        .uncertain()
        .map_err(|_| anyhow::anyhow!("recovery journal unavailable"))?;
    if !unresolved(&credentials, &me, &uncertain)?.is_empty() {
        bail!("receipt outcome unknown; query again or explicitly replay the original request ID");
    }
    let reply = dispatcher
        .act(&executor, task, "receive", ActionArgs::default())
        .await
        .map_err(|_| {
            anyhow::anyhow!("receipt unconfirmed; query again before any new receipt intent")
        })?;
    let dispatch = decode_dispatch(&reply["dispatch"])?;
    dispatch.validate()?;
    if dispatch.task()? != task
        || dispatch.project_ident != credentials.project
        || dispatch.target.as_ref() != Some(&me)
        || dispatch.state != "received"
        || dispatch.claim_id.is_some()
        || dispatch.claimed_at.is_some()
        || dispatch.started_at.is_some()
        || dispatch.execution_id.is_some()
        || reply["task"]["id"] != task.to_string()
        || reply["task"]["project_ident"] != credentials.project
        || reply["task"]["status"] != "todo"
        || !reply["task"]["owner_origin"].is_null()
        || !reply["task"]["owner_agent_id"].is_null()
        || !reply["task"]["started_at"].is_null()
        || !reply["task"]["done_at"].is_null()
    {
        bail!("receipt acknowledgment inconsistent; query again");
    }
    Ok(())
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Hash)]
struct Origin {
    session_id: String,
    instance_id: String,
    provider: String,
    os: String,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq, Hash)]
struct Target {
    origin: Origin,
    generation: String,
}

impl Target {
    fn validate(&self) -> Result<()> {
        canonical_uuid(&self.origin.session_id)?;
        canonical_uuid(&self.origin.instance_id)?;
        canonical_uuid(&self.generation)?;
        if !is_os(&self.origin.os) || !is_provider(&self.origin.provider) {
            bail!("invalid recovery origin platform");
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
struct MachineTarget {
    instance_id: String,
    os: String,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
struct Dispatch {
    task_id: String,
    project_ident: String,
    source_project_ident: String,
    requester: Target,
    target: Option<Target>,
    machine_target: Option<MachineTarget>,
    required_os: Option<String>,
    required_provider: Option<String>,
    state: String,
    claim_id: Option<String>,
    received_at: Option<u64>,
    claimed_at: Option<u64>,
    started_at: Option<u64>,
    finished_at: Option<u64>,
    execution_id: Option<String>,
    cancel_requested_at: Option<u64>,
}

impl Dispatch {
    fn task(&self) -> Result<Uuid> {
        canonical_uuid(&self.task_id)
    }

    fn validate(&self) -> Result<()> {
        self.task()?;
        if !is_project(&self.project_ident) || !is_project(&self.source_project_ident) {
            bail!("invalid recovery project");
        }
        self.requester.validate()?;
        if self.required_os.as_deref().is_some_and(|s| !is_os(s))
            || self
                .required_provider
                .as_deref()
                .is_some_and(|s| !is_provider(s))
        {
            bail!("invalid recovery eligibility constraint");
        }
        if let Some(t) = &self.target {
            t.validate()?;
            if !self.eligible(t) {
                bail!("recovery target conflicts with eligibility constraint");
            }
        }
        if let Some(m) = &self.machine_target {
            canonical_uuid(&m.instance_id)?;
            if !is_os(&m.os)
                || self.required_os.as_ref().is_some_and(|s| s != &m.os)
                || self
                    .target
                    .as_ref()
                    .is_some_and(|t| t.origin.instance_id != m.instance_id || t.origin.os != m.os)
            {
                bail!("recovery machine target conflicts with origin");
            }
        }
        if let Some(id) = &self.claim_id {
            canonical_uuid(id)?;
        }
        if let Some(id) = &self.execution_id {
            canonical_uuid(id)?;
        }
        let pristine = self.claim_id.is_none()
            && self.claimed_at.is_none()
            && self.started_at.is_none()
            && self.execution_id.is_none()
            && self.finished_at.is_none()
            && self.cancel_requested_at.is_none();
        let claimed =
            self.claim_id.is_some() && self.claimed_at.is_some() && self.received_at.is_some();
        let running = claimed && self.started_at.is_some() && self.execution_id.is_some();
        let valid = match self.state.as_str() {
            "pending_machine" => {
                self.target.is_none()
                    && self.machine_target.is_some()
                    && pristine
                    && self.received_at.is_none()
            }
            "pending" => self.target.is_some() && pristine && self.received_at.is_none(),
            "received" => self.target.is_some() && pristine && self.received_at.is_some(),
            "claimed" => {
                self.target.is_some()
                    && claimed
                    && self.started_at.is_none()
                    && self.execution_id.is_none()
                    && self.finished_at.is_none()
            }
            "running" => self.target.is_some() && running && self.finished_at.is_none(),
            "succeeded" | "failed" => {
                self.target.is_some() && running && self.finished_at.is_some()
            }
            "cancelled" => {
                (self.target.is_some() || self.machine_target.is_some())
                    && self.finished_at.is_some()
                    && (self.claim_id.is_none() && self.claimed_at.is_none() || claimed)
                    && (self.started_at.is_none() && self.execution_id.is_none() || running)
            }
            _ => false,
        };
        if !valid {
            bail!("inconsistent recovery dispatch state");
        }
        let times = [
            self.received_at,
            self.claimed_at,
            self.started_at,
            self.finished_at,
        ];
        if times
            .into_iter()
            .flatten()
            .collect::<Vec<_>>()
            .windows(2)
            .any(|w| w[0] > w[1])
        {
            bail!("inconsistent recovery timestamps");
        }
        Ok(())
    }

    fn eligible(&self, me: &Target) -> bool {
        self.required_os.as_ref().is_none_or(|s| s == &me.origin.os)
            && self
                .required_provider
                .as_ref()
                .is_none_or(|s| s == &me.origin.provider)
    }

    fn on_machine(&self, me: &Target) -> bool {
        self.target.as_ref().is_some_and(|t| {
            t.origin.instance_id == me.origin.instance_id && t.origin.os == me.origin.os
        }) || self
            .machine_target
            .as_ref()
            .is_some_and(|m| m.instance_id == me.origin.instance_id && m.os == me.origin.os)
    }
}

#[derive(Deserialize)]
struct History {
    id: u64,
    operation: String,
    dispatch: Dispatch,
    created_at: u64,
    authentication: Authentication,
}

#[derive(Deserialize)]
struct Authentication {
    machine: AuthMachine,
    session: Origin,
    generation: String,
}

#[derive(Deserialize)]
struct AuthMachine {
    project_ident: String,
    instance_id: String,
    os: String,
    credential_id: String,
}

impl History {
    fn validate(&self, project: &str) -> Result<Target> {
        let d = &self.dispatch;
        d.validate()?;
        let a = &self.authentication;
        let origin = Target {
            origin: a.session.clone(),
            generation: a.generation.clone(),
        };
        origin.validate()?;
        let owner = d.target.as_ref() == Some(&origin);
        let requester = d.requester == origin;
        let operation_valid = match self.operation.as_str() {
            "claim" => owner && d.state == "claimed",
            "start" => owner && d.state == "running",
            "handoff_prior_claim" => {
                owner
                    && matches!(
                        d.state.as_str(),
                        "pending" | "received" | "claimed" | "running"
                    )
            }
            "succeeded" | "failed" => owner && d.state == self.operation,
            "cancel" => (owner || requester) && d.state == "cancelled",
            _ => false,
        };
        let auth_project = if owner {
            &d.project_ident
        } else {
            &d.source_project_ident
        };
        if self.id == 0
            || self.created_at == 0
            || !operation_valid
            || d.project_ident != project
            || &a.machine.project_ident != auth_project
            || a.machine.instance_id != a.session.instance_id
            || a.machine.os != a.session.os
            || a.machine.credential_id.len() != 64
            || !a
                .machine
                .credential_id
                .bytes()
                .all(|b| b.is_ascii_hexdigit())
        {
            bail!("inconsistent authenticated recovery history");
        }
        Ok(origin)
    }
}

fn canonical_uuid(s: &str) -> Result<Uuid> {
    let id = Uuid::parse_str(s)
        .ok()
        .filter(|id| !id.is_nil() && id.to_string() == s)
        .context("recovery UUID must be canonical and non-nil")?;
    Ok(id)
}

fn is_project(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 128
        && s != "."
        && s != ".."
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b"-_.".contains(&b))
}
fn is_os(s: &str) -> bool {
    matches!(s, "linux" | "windows" | "macos")
}
fn is_provider(s: &str) -> bool {
    matches!(s, "codex" | "claude")
}

fn list<'a>(value: &'a Value, key: &str, cap: usize) -> Result<&'a [Value]> {
    value[key]
        .as_array()
        .filter(|v| v.len() <= cap)
        .map(Vec::as_slice)
        .context("recovery list missing or too large")
}

fn decode_dispatch(value: &Value) -> Result<Dispatch> {
    // The wire contract explicitly includes these nullable fields. Missing state
    // evidence must not silently become an unclaimed/unstarted dispatch.
    for key in [
        "target",
        "required_os",
        "required_provider",
        "claim_id",
        "received_at",
        "claimed_at",
        "started_at",
        "finished_at",
        "execution_id",
        "cancel_requested_at",
    ] {
        if value.get(key).is_none() {
            bail!("recovery dispatch lacks state evidence");
        }
    }
    serde_json::from_value(value.clone()).map_err(|_| anyhow::anyhow!("invalid recovery dispatch"))
}

fn unresolved(c: &Credentials, me: &Target, requests: &[Request]) -> Result<HashSet<Uuid>> {
    if requests.len() > 512 {
        bail!("recovery journal too large");
    }
    let mut tasks = HashSet::new();
    for r in requests {
        let endpoint = reqwest::Url::parse(&r.gateway).ok().map(|u| u.to_string());
        if r.status == "uncertain"
            && endpoint.as_deref() == Some(&c.endpoint)
            && r.project == c.project
            && r.machine_instance == c.machine_id
            && r.session_id.to_string() == me.origin.session_id
            && r.generation.to_string() == me.generation
            && r.body["operation"] == "receive"
        {
            if r.path.len() != 3 || r.path[0] != "task-dispatches" || r.path[2] != "actions" {
                bail!("uncertain recovery receipt path malformed");
            }
            tasks.insert(canonical_uuid(&r.path[1])?);
        }
    }
    Ok(tasks)
}

fn local_record(c: &Credentials, target: &Target, project: &Path) -> Option<identity::AgentRecord> {
    if target.origin.instance_id != c.machine_id.to_string()
        || target.origin.os != std::env::consts::OS
    {
        return None;
    }
    let path = c
        .state
        .join("agents")
        .join(format!("{}.json", target.origin.session_id));
    let mut bytes = Vec::new();
    std::fs::File::open(path)
        .ok()?
        .take(16_385)
        .read_to_end(&mut bytes)
        .ok()?;
    if bytes.len() > 16_384 {
        return None;
    }
    let record: identity::AgentRecord = serde_json::from_slice(&bytes).ok()?;
    (record.pid != 0
        && record.identity.session_id.to_string() == target.origin.session_id
        && record.identity.instance_id.to_string() == target.generation
        && record.identity.provider.command() == target.origin.provider
        && record.identity.os == target.origin.os
        && record.identity.project_directory == project)
        .then_some(record)
}

fn parse(
    c: &Credentials,
    me: &Target,
    directory: Value,
    snapshot: Value,
    requests: &[Request],
    queried_at: Instant,
    project: &Path,
) -> Result<Recovery> {
    if serde_json::to_vec(&directory)?.len() > 4 * 1024 * 1024
        || serde_json::to_vec(&snapshot)?.len() > 4 * 1024 * 1024
    {
        bail!("recovery response too large");
    }
    if snapshot["history_complete"] != true {
        bail!("complete recovery history unavailable; reminder deferred");
    }
    let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_millis() as u64;
    let mut live = HashSet::new();
    let mut live_sessions = HashSet::new();
    for row in list(&directory, "sessions", 1000)? {
        let t: Target = serde_json::from_value(row["target"].clone())
            .map_err(|_| anyhow::anyhow!("invalid recovery directory target"))?;
        t.validate()?;
        let seen = row["seen_at"]
            .as_u64()
            .context("invalid recovery directory timestamp")?;
        let expires = row["expires_at"]
            .as_u64()
            .context("invalid recovery directory timestamp")?;
        if expires <= now
            || expires <= seen
            || !live_sessions.insert((t.origin.instance_id.clone(), t.origin.session_id.clone()))
            || !live.insert(t)
        {
            bail!("stale or ambiguous recovery directory");
        }
    }
    if !live.contains(me) {
        bail!("current recovery generation absent from live directory");
    }
    let mut docs: HashMap<Uuid, Dispatch> = HashMap::new();
    for row in list(&snapshot, "machine_dispatches", MAX_LIST)? {
        let d = decode_dispatch(row)?;
        d.validate()?;
        if d.project_ident != c.project || !d.on_machine(me) || docs.insert(d.task()?, d).is_some()
        {
            bail!("foreign or duplicate machine recovery dispatch");
        }
    }
    for (key, machine) in [("pending", false), ("machine_pending", true)] {
        let mut ids = HashSet::new();
        for row in list(&snapshot, key, MAX_LIST)? {
            let d = decode_dispatch(row)?;
            d.validate()?;
            let valid = if machine {
                d.state == "pending_machine" && d.on_machine(me)
            } else {
                d.target.as_ref() == Some(me) && matches!(d.state.as_str(), "pending" | "received")
            };
            if !valid || !ids.insert(d.task()?) || docs.get(&d.task()?) != Some(&d) {
                bail!("conflicting recovery pending snapshot");
            }
        }
        let expected: HashSet<_> = docs
            .iter()
            .filter(|(_, d)| {
                if machine {
                    d.state == "pending_machine"
                } else {
                    d.target.as_ref() == Some(me)
                        && matches!(d.state.as_str(), "pending" | "received")
                }
            })
            .map(|(id, _)| *id)
            .collect();
        if ids != expected {
            bail!("incomplete pending recovery snapshot");
        }
    }
    let mut requested_ids = HashSet::new();
    for row in list(&snapshot, "requested", MAX_LIST)? {
        let d = decode_dispatch(row)?;
        d.validate()?;
        if d.source_project_ident != c.project
            || d.requester != *me
            || !requested_ids.insert(d.task()?)
            || docs.get(&d.task()?).is_some_and(|current| current != &d)
        {
            bail!("conflicting requester recovery snapshot");
        }
    }
    let mut histories: HashMap<Uuid, Vec<History>> = HashMap::new();
    let mut last_id = 0;
    for row in list(&snapshot, "prior_claims", MAX_LIST)? {
        decode_dispatch(&row["dispatch"])?;
        let h: History = serde_json::from_value(row.clone())
            .map_err(|_| anyhow::anyhow!("invalid recovery history"))?;
        let origin = h.validate(&c.project)?;
        let task = h.dispatch.task()?;
        if h.id <= last_id
            || origin.origin.instance_id != me.origin.instance_id && !docs.contains_key(&task)
        {
            bail!("foreign or ambiguous recovery history");
        }
        if let Some(current) = docs.get(&task)
            && (current.requester != h.dispatch.requester
                || current.source_project_ident != h.dispatch.source_project_ident
                || current.required_os != h.dispatch.required_os
                || current.required_provider != h.dispatch.required_provider)
        {
            bail!("recovery history conflicts with current task");
        }
        last_id = h.id;
        histories.entry(task).or_default().push(h);
    }
    let unresolved_receipts = unresolved(c, me, requests)?;
    let mut ready = Vec::new();
    let mut awaiting_receipt = Vec::new();
    let mut docs: Vec<_> = docs.into_iter().collect();
    docs.sort_by_key(|(id, _)| *id);
    for (task, d) in docs {
        let exact = d.target.as_ref() == Some(me);
        let machine = d.state == "pending_machine" && d.eligible(me);
        // Leases expire for CLI, busy and consent-off peers too. Require native
        // death evidence, never absence from the gateway directory alone.
        let old = d.target.as_ref().is_some_and(|target| {
            target != me
                && !live.contains(target)
                && local_record(c, target, project).is_some_and(|record| {
                    identity::process_is_gone(record.pid, &record.process_generation)
                })
        })
            && matches!(
                d.state.as_str(),
                "pending" | "received" | "claimed" | "running"
            )
            // Every query of the same directory chooses the same recipient;
            // ordinary pending-machine receipt still races atomically at the server.
            && live.iter().filter(|target| {
                target.origin.instance_id == me.origin.instance_id
                    && target.origin.os == me.origin.os
                    && target.origin.provider == "codex"
                    && d.eligible(target)
            }).min_by_key(|target| (&target.origin.session_id, &target.generation)) == Some(me);
        if !((exact && matches!(d.state.as_str(), "pending" | "received")) || machine || old) {
            continue;
        }
        let history = histories.get(&task).map(Vec::as_slice).unwrap_or_default();
        if matches!(d.state.as_str(), "claimed" | "running")
            && !history.iter().any(|h| {
                h.dispatch.target == d.target
                    && h.dispatch.claim_id == d.claim_id
                    && h.dispatch.execution_id == d.execution_id
            })
        {
            bail!("active recovery task lacks authenticated claim context");
        }
        let work = Work {
            task,
            text: reminder(&d, history, me)?,
        };
        if exact && d.state == "pending" || machine {
            awaiting_receipt.push(work);
        } else {
            ready.push(work);
        }
        if ready.len() + awaiting_receipt.len() > MAX_WORK {
            bail!("too many eligible recovery items; reminder deferred");
        }
    }
    Ok(Recovery {
        ready,
        awaiting_receipt,
        unresolved_receipts,
        observed_at_ms: now,
        queried_at,
    })
}

fn reminder(d: &Dispatch, history: &[History], me: &Target) -> Result<String> {
    let mut text = format!("Task {} project {}. ", d.task_id, d.project_ident);
    let prior = history
        .iter()
        .rev()
        .find(|h| {
            h.dispatch.claim_id.is_some()
                && (h.dispatch.target != d.target || h.dispatch.claim_id != d.claim_id)
        })
        .map(|h| &h.dispatch)
        .or_else(|| (d.target.as_ref() != Some(me)).then_some(d));
    if let Some(old) = prior {
        if let Some(t) = &old.target {
            text.push_str(&format!(
                "Previous session {} machine {} generation {}. ",
                t.origin.session_id, t.origin.instance_id, t.generation
            ));
        }
        if let Some(claim) = &old.claim_id {
            text.push_str(&format!("Previous claim {claim}. "));
        }
        if let Some(execution) = &old.execution_id {
            text.push_str(&format!("Previous execution {execution}. "));
        }
        text.push_str("Previous execution outcome unknown; inspect recorded history. ");
    }
    text.push_str("Check the current task record and existing progress before any claim or execution. This reminder does not authorize restarting work.");
    if !text.is_ascii() || text.len() > 1024 {
        bail!("recovery reminder exceeds safe text bound");
    }
    Ok(text)
}

#[cfg(test)]
mod tests {
    use super::*;
    use amux::identity::Provider;
    use serde_json::json;
    use sha2::{Digest, Sha256};
    use std::{
        io::{Read, Write},
        net::TcpListener,
        thread,
        time::Duration,
    };

    fn parse(
        c: &Credentials,
        me: &Target,
        directory: Value,
        snapshot: Value,
        requests: &[Request],
        queried_at: Instant,
    ) -> Result<Recovery> {
        super::parse(
            c,
            me,
            directory,
            snapshot,
            requests,
            queried_at,
            Path::new(env!("CARGO_MANIFEST_DIR")),
        )
    }

    fn target(machine: Uuid) -> Value {
        json!({"origin": {"session_id": Uuid::new_v4(), "instance_id": machine,
            "provider": "codex", "os": "linux"}, "generation": Uuid::new_v4()})
    }

    fn fixture() -> (Credentials, Target, Value) {
        let machine = Uuid::new_v4();
        let me: Target = serde_json::from_value(target(machine)).unwrap();
        let c = Credentials {
            endpoint: "http://localhost/".into(),
            project: "demo".into(),
            token: "fixture-secret".into(),
            machine_id: machine,
            machine_key: "a".repeat(64),
            state: PathBuf::new(),
        };
        let directory = directory(std::slice::from_ref(&me));
        (c, me, directory)
    }

    fn as_value(t: &Target) -> Value {
        json!({"origin": {"session_id": t.origin.session_id, "instance_id": t.origin.instance_id,
            "os": t.origin.os, "provider": t.origin.provider}, "generation": t.generation})
    }

    fn directory(targets: &[Target]) -> Value {
        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_millis() as u64;
        json!({"sessions": targets.iter().map(|t| json!({"target": as_value(t),
            "seen_at": now, "expires_at": now + 60_000})).collect::<Vec<_>>()})
    }

    fn dispatch(t: Value, state: &str) -> Value {
        json!({"task_id": Uuid::new_v4(), "project_ident": "demo", "source_project_ident": "demo",
            "requester": target(Uuid::new_v4()), "target": t, "machine_target": null,
            "required_os": null, "required_provider": null, "state": state,
            "claim_id": null, "received_at": null, "claimed_at": null,
            "started_at": null, "execution_id": null, "finished_at": null,
            "cancel_requested_at": null,
            "title": "UNTRUSTED TITLE", "specification": "UNTRUSTED SPECIFICATION"})
    }

    fn packet(docs: Vec<Value>, me: &Target, history: Vec<Value>) -> Value {
        json!({"history_complete": true,
            "pending": docs.iter().filter(|d| d["target"] == as_value(me)
                && matches!(d["state"].as_str(), Some("pending" | "received"))).collect::<Vec<_>>(),
            "machine_pending": docs.iter().filter(|d| d["state"] == "pending_machine").collect::<Vec<_>>(),
            "machine_dispatches": docs, "requested": [], "prior_claims": history})
    }

    fn history(d: &Value, id: u64, operation: &str) -> Value {
        json!({"id": id, "created_at": 10 + id, "operation": operation, "dispatch": d,
            "authentication": {"machine": {"project_ident": "demo",
                "instance_id": d["target"]["origin"]["instance_id"], "os": "linux",
                "credential_id": "b".repeat(64)},
                "session": d["target"]["origin"], "generation": d["target"]["generation"]}})
    }

    fn active(mut d: Value) -> Value {
        d["state"] = json!("running");
        d["received_at"] = json!(1);
        d["claimed_at"] = json!(2);
        d["started_at"] = json!(3);
        d["claim_id"] = json!(Uuid::new_v4());
        d["execution_id"] = json!(Uuid::new_v4());
        d
    }

    #[test]
    fn complete_context_is_required_even_for_empty_history() {
        let (c, me, dir) = fixture();
        let mut p = packet(vec![], &me, vec![]);
        p.as_object_mut().unwrap().remove("history_complete");
        assert!(parse(&c, &me, dir.clone(), p.clone(), &[], Instant::now()).is_err());
        for marker in [Value::Null, json!(false), json!("true")] {
            p["history_complete"] = marker;
            assert!(parse(&c, &me, dir.clone(), p.clone(), &[], Instant::now()).is_err());
        }
        p["history_complete"] = json!(true);
        assert!(
            parse(&c, &me, dir, p, &[], Instant::now())
                .unwrap()
                .ready
                .is_empty()
        );
    }

    #[test]
    fn receipt_is_separate_and_untrusted_task_text_never_enters_reminder() {
        let (c, me, dir) = fixture();
        let pending = dispatch(as_value(&me), "pending");
        let mut received = dispatch(as_value(&me), "received");
        received["received_at"] = json!(1);
        let r = parse(
            &c,
            &me,
            dir,
            packet(vec![pending, received], &me, vec![]),
            &[],
            Instant::now(),
        )
        .unwrap();
        assert_eq!(r.awaiting_receipt.len(), 1);
        assert_eq!(r.ready.len(), 1);
        for w in r.ready.iter().chain(&r.awaiting_receipt) {
            assert!(w.text.is_ascii() && w.text.len() <= 1024);
            assert!(
                w.text
                    .contains("existing progress before any claim or execution")
            );
            assert!(!w.text.contains("UNTRUSTED"));
        }
    }

    #[cfg(target_os = "linux")]
    struct NativePeer(std::process::Child);

    #[cfg(target_os = "linux")]
    impl Drop for NativePeer {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }

    #[cfg(target_os = "linux")]
    fn native_peer(c: &Credentials, target: &Target) -> NativePeer {
        use amux::identity::AgentRecord;
        use std::{
            fs,
            process::{Command, Stdio},
        };
        let child = NativePeer(
            Command::new("/bin/sh")
                .args(["-c", "read -r unused"])
                .stdin(Stdio::piped())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .unwrap(),
        );
        let actor = AgentIdentity {
            project_id: Uuid::nil(),
            agent_id: String::new(),
            session_id: target.origin.session_id.parse().unwrap(),
            instance_id: target.generation.parse().unwrap(),
            provider: if target.origin.provider == "claude" {
                Provider::Claude
            } else {
                Provider::Codex
            },
            os: target.origin.os.clone(),
            project_directory: PathBuf::from(env!("CARGO_MANIFEST_DIR")),
        };
        let record = AgentRecord::inspect(actor, child.0.id()).unwrap();
        fs::create_dir_all(c.state.join("agents")).unwrap();
        fs::write(
            c.state
                .join("agents")
                .join(format!("{}.json", target.origin.session_id)),
            serde_json::to_vec(&record).unwrap(),
        )
        .unwrap();
        child
    }

    #[test]
    #[cfg(target_os = "linux")]
    fn live_peer_is_not_recovered_but_unavailable_peer_retains_claim_and_execution() {
        let (mut c, me, _) = fixture();
        let state = test_state();
        c.state = state.path().into();
        let peer: Target = serde_json::from_value(target(c.machine_id)).unwrap();
        let mut native = native_peer(&c, &peer);
        let old = active(dispatch(as_value(&peer), "pending"));
        let p = packet(vec![old.clone()], &me, vec![history(&old, 1, "start")]);
        let r = parse(
            &c,
            &me,
            directory(&[me.clone(), peer.clone()]),
            p.clone(),
            &[],
            Instant::now(),
        )
        .unwrap();
        assert!(r.ready.is_empty() && r.awaiting_receipt.is_empty());
        // Lease expiry/omission is not death: a CLI or consent-off actor can
        // still be working without sending directory heartbeats.
        let r = parse(
            &c,
            &me,
            directory(std::slice::from_ref(&me)),
            p.clone(),
            &[],
            Instant::now(),
        )
        .unwrap();
        assert!(r.ready.is_empty() && r.awaiting_receipt.is_empty());
        native.0.kill().unwrap();
        native.0.wait().unwrap();
        let r = parse(
            &c,
            &me,
            directory(std::slice::from_ref(&me)),
            p,
            &[],
            Instant::now(),
        )
        .unwrap();
        assert_eq!(r.ready.len(), 1);
        let text = &r.ready[0].text;
        for key in ["claim_id", "execution_id"] {
            assert!(text.contains(old[key].as_str().unwrap()));
        }
        assert!(text.contains(&peer.origin.session_id));
        assert!(text.contains("outcome unknown"));
    }

    #[test]
    #[cfg(target_os = "linux")]
    fn unavailable_work_selects_one_peer_and_requires_local_native_proof() {
        let (mut c, me, _) = fixture();
        let state = test_state();
        c.state = state.path().into();
        let peer: Target = serde_json::from_value(target(c.machine_id)).unwrap();
        let other: Target = serde_json::from_value(target(c.machine_id)).unwrap();
        let old = active(dispatch(as_value(&peer), "pending"));
        let history = vec![history(&old, 1, "start")];
        let dir = directory(&[other.clone(), me.clone()]);
        // A missing/corrupt local record never becomes death evidence.
        for who in [&me, &other] {
            let r = parse(
                &c,
                who,
                dir.clone(),
                packet(vec![old.clone()], who, history.clone()),
                &[],
                Instant::now(),
            )
            .unwrap();
            assert!(r.ready.is_empty());
        }
        let mut native = native_peer(&c, &peer);
        native.0.kill().unwrap();
        native.0.wait().unwrap();
        let first = parse(
            &c,
            &me,
            dir.clone(),
            packet(vec![old.clone()], &me, history.clone()),
            &[],
            Instant::now(),
        )
        .unwrap();
        let second = parse(
            &c,
            &other,
            directory(&[me.clone(), other.clone()]),
            packet(vec![old.clone()], &other, history.clone()),
            &[],
            Instant::now(),
        )
        .unwrap();
        assert_eq!(first.ready.len() + second.ready.len(), 1);
        let selected = if me.origin.session_id < other.origin.session_id {
            &first
        } else {
            &second
        };
        assert_eq!(selected.ready.len(), 1);
        assert!(first.awaiting_receipt.is_empty() && second.awaiting_receipt.is_empty());
        let path = c
            .state
            .join("agents")
            .join(format!("{}.json", peer.origin.session_id));
        std::fs::write(&path, b"{not valid JSON").unwrap();
        assert!(
            parse(
                &c,
                &me,
                dir,
                packet(vec![old], &me, history),
                &[],
                Instant::now()
            )
            .unwrap()
            .ready
            .is_empty()
        );
    }

    #[test]
    fn handoff_keeps_authenticated_other_machine_lineage() {
        let (c, me, dir) = fixture();
        let old = active(dispatch(target(Uuid::new_v4()), "pending"));
        let mut current = dispatch(as_value(&me), "pending");
        for key in ["task_id", "requester"] {
            current[key] = old[key].clone();
        }
        let p = packet(
            vec![current],
            &me,
            vec![
                history(&old, 1, "start"),
                history(&old, 2, "handoff_prior_claim"),
            ],
        );
        let r = parse(&c, &me, dir.clone(), p.clone(), &[], Instant::now()).unwrap();
        let text = &r.awaiting_receipt[0].text;
        assert!(text.contains(old["target"]["origin"]["instance_id"].as_str().unwrap()));
        assert!(text.contains(old["claim_id"].as_str().unwrap()));
        let mut forged = p;
        forged["prior_claims"][0]["authentication"]["generation"] = json!(Uuid::new_v4());
        assert!(parse(&c, &me, dir, forged, &[], Instant::now()).is_err());
    }

    #[test]
    fn machine_queue_filters_provider_and_requires_no_implicit_claim_or_binding() {
        let (c, me, dir) = fixture();
        let mut d = dispatch(Value::Null, "pending_machine");
        d["machine_target"] = json!({"instance_id": c.machine_id, "os": "linux"});
        d["required_provider"] = json!("claude");
        assert!(
            parse(
                &c,
                &me,
                dir.clone(),
                packet(vec![d.clone()], &me, vec![]),
                &[],
                Instant::now()
            )
            .unwrap()
            .awaiting_receipt
            .is_empty()
        );
        d["required_provider"] = json!("codex");
        assert_eq!(
            parse(
                &c,
                &me,
                dir.clone(),
                packet(vec![d.clone()], &me, vec![]),
                &[],
                Instant::now()
            )
            .unwrap()
            .awaiting_receipt
            .len(),
            1
        );
        d["claim_id"] = json!(Uuid::new_v4());
        assert!(
            parse(
                &c,
                &me,
                dir,
                packet(vec![d], &me, vec![]),
                &[],
                Instant::now()
            )
            .is_err()
        );
    }

    #[test]
    fn malformed_foreign_duplicate_and_inconsistent_packets_defer() {
        let (c, me, dir) = fixture();
        let d = dispatch(as_value(&me), "pending");
        for (key, value) in [
            ("task_id", json!(Uuid::nil())),
            ("task_id", json!(Uuid::new_v4().to_string().to_uppercase())),
            ("project_ident", json!("other")),
            ("state", json!("running")),
            ("source_project_ident", json!("Bad\nProject")),
            ("required_provider", json!("unknown")),
        ] {
            let mut bad = d.clone();
            bad[key] = value;
            assert!(
                parse(
                    &c,
                    &me,
                    dir.clone(),
                    packet(vec![bad], &me, vec![]),
                    &[],
                    Instant::now()
                )
                .is_err(),
                "{key}"
            );
        }
        assert!(
            parse(
                &c,
                &me,
                dir.clone(),
                packet(vec![d.clone(), d.clone()], &me, vec![]),
                &[],
                Instant::now()
            )
            .is_err()
        );
        let mut p = packet(vec![d], &me, vec![]);
        p["pending"] = json!([]);
        assert!(parse(&c, &me, dir.clone(), p, &[], Instant::now()).is_err());
        let foreign = dispatch(target(Uuid::new_v4()), "pending");
        assert!(
            parse(
                &c,
                &me,
                dir,
                packet(vec![foreign], &me, vec![]),
                &[],
                Instant::now()
            )
            .is_err()
        );
    }

    #[test]
    fn too_many_eligible_items_and_directory_ambiguity_defer() {
        let (c, me, dir) = fixture();
        let p = packet(
            (0..129)
                .map(|_| dispatch(as_value(&me), "pending"))
                .collect(),
            &me,
            vec![],
        );
        assert!(parse(&c, &me, dir, p, &[], Instant::now()).is_err());
        let mut other = me.clone();
        other.generation = Uuid::new_v4().to_string();
        assert!(
            parse(
                &c,
                &me,
                directory(&[me.clone(), other]),
                packet(vec![], &me, vec![]),
                &[],
                Instant::now()
            )
            .is_err()
        );
        let mut p = packet(vec![], &me, vec![]);
        p["prior_claims"] = json!(vec![Value::Null; MAX_LIST + 1]);
        assert!(
            parse(
                &c,
                &me,
                directory(std::slice::from_ref(&me)),
                p,
                &[],
                Instant::now()
            )
            .is_err()
        );
    }

    fn intent(c: &Credentials, me: &Target, task: Uuid) -> Request {
        let request_id = Uuid::new_v4();
        Request {
            request_id,
            gateway: c.endpoint.clone(),
            project: c.project.clone(),
            machine_instance: c.machine_id,
            session_id: me.origin.session_id.parse().unwrap(),
            generation: me.generation.parse().unwrap(),
            path: vec!["task-dispatches".into(), task.to_string(), "actions".into()],
            body: json!({"request_id": request_id, "generation": me.generation, "operation": "receive"}),
            status: "uncertain".into(),
            response: None,
            error: None,
        }
    }

    #[test]
    fn preferences_origin_normalization_keeps_uncertain_receipts_fenced() {
        let (previous, me, _) = fixture();
        for (entered, canonical) in [
            ("https://gw.example", "https://gw.example/"),
            ("https://GW.EXAMPLE:443", "https://gw.example/"),
            ("http://LOCALHOST:80", "http://localhost/"),
        ] {
            let preferences = amux::gateway_preferences::GatewayPreferences {
                automatic_notifications: false,
                url: entered.into(),
                api_key: previous.token.clone(),
                machine_id: previous.machine_id.to_string(),
                machine_key: previous.machine_key.clone(),
            };
            let credentials =
                Credentials::from_preferences(&previous.project, &preferences).unwrap();
            let task = Uuid::new_v4();
            let mut uncertain = intent(&credentials, &me, task);
            // The dispatcher always writes its parsed, canonical origin to disk.
            uncertain.gateway = canonical.into();
            assert_eq!(
                unresolved(&credentials, &me, &[uncertain]).unwrap(),
                HashSet::from([task]),
                "uncertain receipt was lost for {entered}"
            );
            assert_eq!(credentials.endpoint, canonical);
        }
    }

    #[test]
    fn unresolved_receipts_are_scoped_and_fresh_reads_do_not_rewrite_unknown_intents() {
        let (c, me, dir) = fixture();
        let mut d = dispatch(as_value(&me), "received");
        d["received_at"] = json!(1);
        let task = canonical_uuid(d["task_id"].as_str().unwrap()).unwrap();
        let own = intent(&c, &me, task);
        let mut peer = own.clone();
        peer.session_id = Uuid::new_v4();
        let mut elsewhere = own.clone();
        elsewhere.gateway = "https://elsewhere/".into();
        let mut generation = own.clone();
        generation.generation = Uuid::new_v4();
        let r = parse(
            &c,
            &me,
            dir,
            packet(vec![d], &me, vec![]),
            &[own.clone(), peer, elsewhere, generation],
            Instant::now(),
        )
        .unwrap();
        assert_eq!(r.unresolved_receipts, HashSet::from([task]));
        assert_eq!(r.ready[0].task, task);
        assert_eq!(own.status, "uncertain");
    }

    fn test_state() -> tempfile::TempDir {
        let base = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/desktop-recovery-helper");
        std::fs::create_dir_all(&base).unwrap();
        tempfile::tempdir_in(base).unwrap()
    }

    type SeenRequests = Vec<(String, Value)>;

    fn server(replies: Vec<Value>) -> (String, thread::JoinHandle<(SeenRequests, TcpListener)>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let url = format!("http://{}/", listener.local_addr().unwrap());
        let handle = thread::spawn(move || {
            let mut seen = Vec::new();
            for ack in replies {
                let deadline = Instant::now() + Duration::from_secs(10);
                let mut stream = loop {
                    match listener.accept() {
                        Ok((s, _)) => break s,
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                            assert!(Instant::now() < deadline);
                            thread::sleep(Duration::from_millis(5));
                        }
                        Err(e) => panic!("{e}"),
                    }
                };
                stream.set_nonblocking(false).unwrap();
                stream
                    .set_read_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                let mut bytes = Vec::new();
                let (end, length) = loop {
                    let mut b = [0; 4096];
                    let n = stream.read(&mut b).unwrap();
                    assert!(n > 0);
                    bytes.extend_from_slice(&b[..n]);
                    if let Some(end) = bytes.windows(4).position(|w| w == b"\r\n\r\n") {
                        let h = String::from_utf8_lossy(&bytes[..end]).to_lowercase();
                        let length: usize = h
                            .lines()
                            .find_map(|l| l.strip_prefix("content-length: ")?.parse().ok())
                            .unwrap_or(0);
                        break (end + 4, length);
                    }
                };
                while bytes.len() < end + length {
                    let mut b = [0; 4096];
                    let n = stream.read(&mut b).unwrap();
                    assert!(n > 0);
                    bytes.extend_from_slice(&b[..n]);
                }
                let head = String::from_utf8_lossy(&bytes[..end]).into_owned();
                let body: Value =
                    serde_json::from_slice(&bytes[end..end + length]).unwrap_or(Value::Null);
                seen.push((head, body));
                let ack = serde_json::to_vec(&ack).unwrap();
                write!(stream, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n", ack.len()).unwrap();
                stream.write_all(&ack).unwrap();
            }
            (seen, listener)
        });
        (url, handle)
    }

    #[tokio::test]
    async fn query_only_heartbeats_and_reads_complete_context() {
        let (mut c, me, dir) = fixture();
        let state = test_state();
        c.state = state.path().into();
        let actor = AgentIdentity {
            project_id: Uuid::nil(),
            agent_id: String::new(),
            session_id: me.origin.session_id.parse().unwrap(),
            instance_id: me.generation.parse().unwrap(),
            provider: Provider::Codex,
            os: "linux".into(),
            project_directory: PathBuf::from(env!("CARGO_MANIFEST_DIR")),
        };
        let d = dispatch(as_value(&me), "pending");
        let registration =
            json!({"target": as_value(&me), "expires_at": dir["sessions"][0]["expires_at"]});
        let (url, server) = server(vec![registration, dir, packet(vec![d], &me, vec![])]);
        c.endpoint = url;
        let r = query(c.clone(), actor).await.unwrap();
        assert_eq!(r.awaiting_receipt.len(), 1);
        assert!(r.ready.is_empty());
        let (seen, listener) = server.join().unwrap();
        assert_eq!(seen.len(), 3);
        assert!(
            seen[0]
                .0
                .starts_with("POST /v1/projects/demo/task-sessions ")
        );
        assert_eq!(seen[0].1, json!({"generation": me.generation}));
        assert!(
            seen[1]
                .0
                .starts_with("GET /v1/projects/demo/task-sessions ")
        );
        assert!(seen[2].0.starts_with(&format!(
            "GET /v1/projects/demo/task-dispatches/recovery?generation={} ",
            me.generation
        )));
        assert!(seen[1].1.is_null() && seen[2].1.is_null());
        assert!(matches!(listener.accept(), Err(e) if e.kind() == std::io::ErrorKind::WouldBlock));
        assert!(!c.state.join("dispatch/journal.json").exists());
        // Both transport and journal share the state parent; there is no agents subdirectory.
        assert!(c.state.join("dispatch/keys").is_dir());
        assert!(!c.state.join("agents").exists());
    }

    #[tokio::test]
    async fn receive_posts_only_a_receipt_and_blocks_any_existing_unknown_receipt() {
        let (mut c, me, _) = fixture();
        let state = test_state();
        c.state = state.path().into();
        let actor = AgentIdentity {
            project_id: Uuid::nil(),
            agent_id: String::new(),
            session_id: me.origin.session_id.parse().unwrap(),
            instance_id: me.generation.parse().unwrap(),
            provider: Provider::Codex,
            os: "linux".into(),
            project_directory: PathBuf::from(env!("CARGO_MANIFEST_DIR")),
        };
        let mut d = dispatch(as_value(&me), "received");
        d["received_at"] = json!(1);
        let task = canonical_uuid(d["task_id"].as_str().unwrap()).unwrap();
        let ack = json!({"dispatch": d, "task": {"id": task, "project_ident": "demo", "status": "todo"},
            "authentication": {"session": as_value(&me)["origin"], "generation": me.generation,
                "machine": {"instance_id": c.machine_id, "project_ident": "demo", "os": "linux",
                    "credential_id": format!("{:x}", Sha256::digest(c.machine_key.as_bytes()))}}});
        let (url, server) = server(vec![ack]);
        c.endpoint = url;
        receive(c.clone(), actor.clone(), task).await.unwrap();
        let (seen, listener) = server.join().unwrap();
        assert_eq!(seen.len(), 1);
        let (head, body) = &seen[0];
        assert!(head.starts_with(&format!(
            "POST /v1/projects/demo/task-dispatches/{task}/actions "
        )));
        assert_eq!(body["operation"], "receive");
        for field in [
            "claim_id",
            "execution_id",
            "result",
            "target",
            "machine_target",
        ] {
            assert!(body[field].is_null());
        }
        // Simulate an earlier lost response without sending any request to a gateway.
        let unknown = intent(&c, &me, Uuid::new_v4());
        let journal = c.state.join("dispatch/journal.json");
        std::fs::write(
            &journal,
            serde_json::to_vec(&json!({"version": 1, "requests": [unknown]})).unwrap(),
        )
        .unwrap();
        let before = std::fs::read(&journal).unwrap();
        assert!(receive(c, actor, task).await.is_err());
        assert_eq!(std::fs::read(&journal).unwrap(), before);
        assert!(matches!(listener.accept(), Err(e) if e.kind() == std::io::ErrorKind::WouldBlock));
    }
}
