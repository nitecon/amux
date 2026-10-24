//! Private local registry binding verified provider-native actors to published terminals.
//! The delivery session of a target is amux's process identity; an actor's origin is the
//! provider-native logical conversation. They are never derived from one another.
use crate::{
    delegation::{Origin, Target, normalize_goal},
    identity::{self, Provider},
};
use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    io::{Read, Write},
    path::Path,
};
use sysinfo::{Pid, ProcessRefreshKind, ProcessesToUpdate, System, UpdateKind};
use uuid::Uuid;

const MAX_TARGETS: usize = 64;
const MAX_ACTORS: usize = 64;
const MAX_FILE: usize = 64 * 1024;
const MAX_DEPTH: usize = 64;
/// Published context and announcement clocks, in milliseconds.
const FRESH_MS: i64 = 5_000;
const SKEW_MS: i64 = 30_000;

/// One published terminal attachment and the provider amux verified as its outer application.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ActorTarget {
    pub target: Target,
    pub provider: Provider,
}

/// Client-owned registration, matching the CMUX gateway actor contract v1 and v2.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Actor {
    #[serde(default = "legacy_version")]
    pub version: u8,
    pub origin: Origin,
    pub provider_session_id: String,
    #[serde(default)]
    pub executor_generation: Vec<String>,
    #[serde(default)]
    pub base_id: Option<String>,
    #[serde(default)]
    pub session_slot: Option<u32>,
    #[serde(default)]
    pub actor_id: Option<String>,
    #[serde(default)]
    pub goal: Option<String>,
}

fn legacy_version() -> u8 {
    1
}

/// Normalize a bounded native conversation identifier.
pub fn native_id(value: &str) -> Result<String> {
    ensure!(
        !value.is_empty() && value.len() <= 256 && value.bytes().all(|b| b.is_ascii_graphic()),
        "Invalid provider session identity"
    );
    Ok(Uuid::parse_str(value)
        .map(|id| id.to_string())
        .unwrap_or_else(|_| value.to_owned()))
}

impl Actor {
    /// Recompute the public contract, rejecting identities inconsistent with their inputs.
    pub fn validate(&self) -> Result<()> {
        let origin = &self.origin;
        ensure!(
            !origin.session_id.is_nil() && !origin.instance_id.is_nil(),
            "Invalid actor origin"
        );
        ensure!(
            matches!(origin.os.as_str(), "linux" | "windows" | "macos"),
            "Invalid actor OS"
        );
        ensure!(
            normalize_goal(self.goal.as_deref())? == self.goal,
            "Invalid normalized goal"
        );
        let native = native_id(&self.provider_session_id)?;
        if self.version == 2 {
            let base_id = self
                .base_id
                .as_deref()
                .context("Missing registration base identity")?;
            let base = Uuid::parse_str(base_id).context("Invalid registration base identity")?;
            ensure!(
                !base.is_nil() && base.to_string() == base_id,
                "Invalid registration base identity"
            );
            let slot = self
                .session_slot
                .filter(|slot| *slot > 0)
                .context("Invalid numbered session registration")?;
            ensure!(
                self.executor_generation.is_empty(),
                "Invalid numbered session registration"
            );
            ensure!(
                self.actor_id.as_deref() == Some(format!("{base_id}-{slot}").as_str()),
                "Readable actor identity does not match its registration"
            );
            let name = json!(["agent-tools-actor-v2", native]).to_string();
            ensure!(
                Uuid::new_v5(&base, name.as_bytes()) == origin.session_id,
                "Actor identity does not match its registered conversation"
            );
            return Ok(());
        }
        ensure!(
            self.version == 1
                && self.base_id.is_none()
                && self.session_slot.is_none()
                && self.actor_id.is_none(),
            "Unsupported actor registration"
        );
        ensure!(
            !self.executor_generation.is_empty()
                && self.executor_generation.len() <= 4
                && self
                    .executor_generation
                    .iter()
                    .all(|v| v.len() <= 128 && v.bytes().all(|b| b.is_ascii_graphic())),
            "Invalid executor generation"
        );
        let name = json!([
            "agent-tools-actor-v1",
            origin.os,
            origin.provider,
            native,
            self.executor_generation
        ])
        .to_string();
        ensure!(
            Uuid::new_v5(&origin.instance_id, name.as_bytes()) == origin.session_id,
            "Actor identity does not match its namespace and runtime"
        );
        Ok(())
    }

    fn describe(&self, value: &mut Value) {
        value["version"] = json!(self.version);
        value["origin"] = json!(self.origin);
        value["provider_session_id"] = json!(native_id(&self.provider_session_id).ok());
        value["goal"] = json!(self.goal);
        if self.version == 2 {
            value["base_id"] = json!(self.base_id);
            value["session_slot"] = json!(self.session_slot);
            value["actor_id"] = json!(self.actor_id);
        } else {
            value["executor_generation"] = json!(self.executor_generation);
        }
    }
}

#[derive(Debug, Serialize, Deserialize)]
struct Published {
    instance: Uuid,
    published_at: i64,
    targets: Vec<ActorTarget>,
}

#[derive(Debug, Serialize, Deserialize)]
struct Record {
    actor: Actor,
    target: Target,
    provider: Provider,
    instance: Uuid,
    announced_at: i64,
}

/// Publish the desktop's current attachments for local announcement callers.
pub fn publish(directory: &Path, instance: Uuid, targets: &[ActorTarget], now: i64) -> Result<()> {
    ensure!(!instance.is_nil(), "Invalid delivery instance");
    ensure!(
        targets.len() <= MAX_TARGETS,
        "Too many published actor targets"
    );
    for at in targets {
        let target = &at.target;
        ensure!(
            !target.session.is_nil() && target.pid > 0 && !target.generation.is_empty(),
            "Invalid published actor target"
        );
    }
    let published = Published {
        instance,
        published_at: now,
        targets: targets.to_vec(),
    };
    write_private(directory, "context.json", &serde_json::to_vec(&published)?)
}

/// Register the calling invocation's own native actor on its exact published terminal.
pub fn announce(directory: &Path, actor: Actor, now: i64) -> Result<Value> {
    actor.validate()?;
    ensure!(
        actor.origin.os == std::env::consts::OS,
        "Actor OS does not match local host"
    );
    let published = load_context(directory, now)?;
    let caller = resolve(&published)?;
    ensure!(
        caller.target.provider == actor.origin.provider,
        "Actor provider does not match its terminal application"
    );
    let native = native_id(&actor.provider_session_id)?;
    ensure!(
        caller_native(actor.origin.provider)? == native,
        "Caller's provider session does not match the actor"
    );
    if actor.version == 1 {
        ensure!(
            v1_generation(caller.executor.0, &caller.executor.1).as_ref()
                == Some(&actor.executor_generation),
            "Actor executor generation is stale or mismatched"
        );
    }
    // Keep the registry bounded to live, currently published attachments.
    let mut kept = 0;
    for (path, record) in records(directory)? {
        let current = record.instance == published.instance
            && published
                .targets
                .iter()
                .any(|at| record.target.same_attachment(&at.target))
            && live(&record.target);
        if !current {
            let _ = std::fs::remove_file(path);
        } else if record.actor.origin.session_id != actor.origin.session_id {
            kept += 1;
        }
    }
    ensure!(kept < MAX_ACTORS, "Too many registered actors");
    let record = Record {
        actor,
        target: caller.target.target.clone(),
        provider: caller.target.provider,
        instance: published.instance,
        announced_at: now,
    };
    write_private(
        &directory.join("actors"),
        &format!("{}.json", record.actor.origin.session_id),
        &serde_json::to_vec(&record)?,
    )?;
    let actors = attached(
        &records(directory)?,
        published.instance,
        &caller.target,
        now,
    );
    let mut reply = attachment(&caller.target, published.instance);
    record.actor.describe(&mut reply);
    reply["binding_state"] = json!(if actors.len() == 1 {
        "bound"
    } else {
        "ambiguous"
    });
    Ok(reply)
}

/// The calling invocation's own terminal and its currently registered actor, if any.
pub fn context(directory: &Path, now: i64) -> Result<Value> {
    let published = load_context(directory, now)?;
    let caller = resolve(&published)?;
    let native = caller_native(caller.target.provider)?;
    let actors = attached(
        &records(directory)?,
        published.instance,
        &caller.target,
        now,
    );
    let mut reply = attachment(&caller.target, published.instance);
    match actors.as_slice() {
        [] => reply["binding_state"] = json!("unbound"),
        [actor] if native_id(&actor.provider_session_id).ok().as_ref() == Some(&native) => {
            actor.describe(&mut reply);
            reply["binding_state"] = json!("bound");
        }
        [_] => reply["binding_state"] = json!("unbound"),
        _ => reply["binding_state"] = json!("ambiguous"),
    }
    Ok(reply)
}

/// Live published terminals and their binding state, without delivery sessions or credentials.
pub fn peers(directory: &Path, now: i64) -> Result<Value> {
    let published = load_context(directory, now)?;
    let records = records(directory)?;
    let peers: Vec<Value> = published
        .targets
        .iter()
        .filter(|at| live(&at.target))
        .map(|at| {
            let actors = attached(&records, published.instance, at, now);
            let mut peer = json!({
                "workspace": at.target.workspace,
                "terminal": at.target.terminal,
                "provider": at.provider,
                "project": at.target.project,
            });
            match actors.as_slice() {
                [] => peer["binding_state"] = json!("unbound"),
                [actor] => {
                    peer["binding_state"] = json!("bound");
                    peer["origin"] = json!(actor.origin);
                    peer["actor_id"] = json!(actor.actor_id);
                    peer["goal"] = json!(actor.goal);
                }
                _ => peer["binding_state"] = json!("ambiguous"),
            }
            peer
        })
        .collect();
    Ok(json!({"instance": published.instance, "peers": peers}))
}

/// Exactly one registered native actor per live attachment, keyed by delivery session.
/// Several logical origins on the same executor are ambiguous and produce no binding.
pub fn bindings(
    directory: &Path,
    instance: Uuid,
    targets: &[ActorTarget],
    now: i64,
) -> Result<Vec<(Uuid, Actor)>> {
    let records = records(directory)?;
    Ok(targets
        .iter()
        .filter(|at| live(&at.target))
        .filter_map(|at| {
            let [actor] = <[Actor; 1]>::try_from(attached(&records, instance, at, now)).ok()?;
            Some((at.target.session, actor))
        })
        .collect())
}

/// Distinct valid logical origins registered on exactly this attachment.
fn attached(
    records: &[(std::path::PathBuf, Record)],
    instance: Uuid,
    at: &ActorTarget,
    now: i64,
) -> Vec<Actor> {
    let mut actors: Vec<Actor> = Vec::new();
    for (_, record) in records {
        if record.instance == instance
            && record.target.same_attachment(&at.target)
            && record.provider == at.provider
            && record.actor.origin.provider == at.provider
            && record.announced_at <= now.saturating_add(SKEW_MS)
            && record.actor.validate().is_ok()
            && !actors
                .iter()
                .any(|a| a.origin.session_id == record.actor.origin.session_id)
        {
            actors.push(record.actor.clone());
        }
    }
    actors
}

fn attachment(at: &ActorTarget, instance: Uuid) -> Value {
    json!({
        "instance": instance,
        "workspace": at.target.workspace,
        "terminal": at.target.terminal,
        "provider": at.provider,
        "project": at.target.project,
    })
}

fn load_context(directory: &Path, now: i64) -> Result<Published> {
    let bytes = read_bounded(&directory.join("context.json"))?
        .context("No published amux terminal context")?;
    let published: Published = serde_json::from_slice(&bytes)?;
    ensure!(
        published.targets.len() <= MAX_TARGETS
            && now.abs_diff(published.published_at) <= FRESH_MS as u64,
        "Published amux terminal context is stale"
    );
    Ok(published)
}

fn records(directory: &Path) -> Result<Vec<(std::path::PathBuf, Record)>> {
    let entries = match std::fs::read_dir(directory.join("actors")) {
        Ok(entries) => entries,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(e.into()),
    };
    let mut paths = Vec::new();
    for entry in entries {
        let path = entry?.path();
        if path.extension().is_some_and(|ext| ext == "json") {
            paths.push(path);
            ensure!(paths.len() <= MAX_ACTORS, "Too many registered actors");
        }
    }
    ensure!(paths.len() <= MAX_ACTORS, "Too many registered actors");
    paths.sort();
    Ok(paths
        .into_iter()
        .filter_map(|path| {
            let record = serde_json::from_slice(&read_bounded(&path).ok()??).ok()?;
            Some((path, record))
        })
        .collect())
}

fn read_bounded(path: &Path) -> Result<Option<Vec<u8>>> {
    let file = match std::fs::File::open(path) {
        Ok(file) => file,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e.into()),
    };
    let mut bytes = Vec::new();
    file.take(MAX_FILE as u64 + 1).read_to_end(&mut bytes)?;
    ensure!(
        bytes.len() <= MAX_FILE,
        "Actor registry file exceeds 64 KiB"
    );
    Ok(Some(bytes))
}

/// Atomic owner-only write; replaced files never expose partial JSON.
fn write_private(directory: &Path, name: &str, bytes: &[u8]) -> Result<()> {
    ensure!(
        bytes.len() <= MAX_FILE,
        "Actor registry file exceeds 64 KiB"
    );
    let mut builder = std::fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
        builder.mode(0o700);
        builder.create(directory)?;
        std::fs::set_permissions(directory, std::fs::Permissions::from_mode(0o700))?;
    }
    #[cfg(not(unix))]
    builder.create(directory)?;
    // Temporary files are created owner-only (0600 on Unix).
    let mut file = tempfile::NamedTempFile::new_in(directory)?;
    file.write_all(bytes)?;
    file.as_file().sync_all()?;
    file.persist(directory.join(name)).map_err(|e| e.error)?;
    Ok(())
}

fn live(target: &Target) -> bool {
    identity::process_generation(target.pid).is_ok_and(|g| g == target.generation)
}

/// The CMUX v1 executor generation for an amux native process generation.
fn v1_generation(pid: u32, generation: &str) -> Option<Vec<String>> {
    if let Some((boot, ticks)) = generation
        .strip_prefix("linux:")
        .and_then(|rest| rest.split_once(':'))
    {
        return Some(vec![
            "linux-proc-v1".into(),
            boot.into(),
            pid.to_string(),
            ticks.into(),
        ]);
    }
    let ticks = generation.strip_prefix("windows:")?;
    Some(vec![
        "windows-process-v1".into(),
        pid.to_string(),
        ticks.into(),
    ])
}

/// Only the calling invocation's own provider variables, never an ancestor's environment.
fn caller_native(provider: Provider) -> Result<String> {
    let keys: &[&str] = match provider {
        Provider::Codex => &["CODEX_THREAD_ID", "CODEX_SESSION_ID"],
        Provider::Claude => &["CLAUDE_CODE_SESSION_ID"],
    };
    let mut found: Option<String> = None;
    for key in keys {
        if let Ok(value) = std::env::var(key) {
            let value = native_id(&value)?;
            ensure!(
                found.as_ref().is_none_or(|f| *f == value),
                "Caller provider session variables disagree"
            );
            found = Some(value);
        }
    }
    found.context("Caller has no provider-native conversation identity")
}

struct Caller {
    target: ActorTarget,
    /// The single provider executor between caller and target: pid and native generation.
    executor: (u32, String),
}

/// Two independent ancestry snapshots must agree on one target and one executor, with no
/// process on the owned chain replaced in between.
fn resolve(published: &Published) -> Result<Caller> {
    let first = walk(published)?;
    let second = walk(published)?;
    ensure!(
        first == second,
        "Caller ancestry changed during verification"
    );
    let (chain, index, executor) = first;
    ensure!(
        chain
            .iter()
            .all(|(pid, generation)| identity::process_generation(*pid)
                .is_ok_and(|g| g == *generation)),
        "Caller ancestry changed during verification"
    );
    let target = published
        .targets
        .iter()
        .find(|at| at.target.pid == chain[index].0 && at.target.generation == chain[index].1)
        .cloned()
        .context("Caller is not inside a published amux terminal")?;
    ensure!(
        executor.2 == target.provider,
        "Caller's provider executor does not match its terminal application"
    );
    Ok(Caller {
        target,
        executor: (executor.0, executor.1),
    })
}

type Walk = (Vec<(u32, String)>, usize, (u32, String, Provider));

fn walk(published: &Published) -> Result<Walk> {
    let mut system = System::new();
    system.refresh_processes_specifics(
        ProcessesToUpdate::All,
        true,
        ProcessRefreshKind::nothing()
            .without_tasks()
            .with_exe(UpdateKind::Always)
            .with_cmd(UpdateKind::Always),
    );
    let mut chain: Vec<(u32, Option<String>, Option<Provider>)> = Vec::new();
    let mut pid = Some(std::process::id());
    while let Some(current) = pid {
        ensure!(chain.len() < MAX_DEPTH, "Caller ancestry is too deep");
        let Some(process) = system.process(Pid::from_u32(current)) else {
            break;
        };
        chain.push((
            current,
            identity::process_generation(current).ok(),
            identity::executor_provider(process),
        ));
        pid = process.parent().map(Pid::as_u32);
    }
    let hits: Vec<usize> = chain
        .iter()
        .enumerate()
        .filter(|(_, (pid, generation, _))| {
            generation.as_ref().is_some_and(|generation| {
                published
                    .targets
                    .iter()
                    .any(|at| at.target.pid == *pid && at.target.generation == *generation)
            })
        })
        .map(|(index, _)| index)
        .collect();
    let [index] = <[usize; 1]>::try_from(hits)
        .ok()
        .context("Caller is not inside exactly one published amux terminal")?;
    let owned = chain[..=index]
        .iter()
        .map(|(pid, generation, _)| Some((*pid, generation.clone()?)))
        .collect::<Option<Vec<_>>>()
        .context("Caller ancestry is unavailable")?;
    let executors: Vec<_> = chain[..=index]
        .iter()
        .filter_map(|(pid, generation, provider)| Some((*pid, generation.clone()?, (*provider)?)))
        .collect();
    let [executor] = <[_; 1]>::try_from(executors).ok().context(
        "Caller does not resolve to exactly one provider executor; nested providers are ambiguous",
    )?;
    Ok((owned, index, executor))
}
