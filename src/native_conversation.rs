//! Bind provider conversations to exact Amux terminal identities, never directory history.
use crate::identity::{AgentIdentity, Provider};
use anyhow::{Context, Result, ensure};
use serde::{Deserialize, Serialize};
use std::{
    io::{Read, Write},
    path::{Path, PathBuf},
};
use uuid::Uuid;

#[derive(Serialize, Deserialize)]
struct Binding {
    conversation: Uuid,
}
fn path(root: &Path, actor: &AgentIdentity) -> PathBuf {
    root.join("native-conversations")
        .join(format!("{}.json", actor.session_id))
}
pub fn read(root: &Path, actor: &AgentIdentity) -> Result<Option<Uuid>> {
    let path = path(root, actor);
    match std::fs::read(&path) {
        Ok(bytes) => Ok(Some(
            serde_json::from_slice::<Binding>(&bytes)
                .context("Invalid native conversation binding")?
                .conversation,
        )),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(e) => Err(e.into()),
    }
}
pub fn save(root: &Path, actor: &AgentIdentity, conversation: Uuid) -> Result<()> {
    ensure!(!conversation.is_nil(), "Native conversation cannot be nil");
    let path = path(root, actor);
    let dir = path.parent().unwrap();
    std::fs::create_dir_all(dir)?;
    let mut file = tempfile::NamedTempFile::new_in(dir)?;
    serde_json::to_writer(&mut file, &Binding { conversation })?;
    file.flush()?;
    file.persist(path).map_err(|e| e.error)?;
    Ok(())
}
/// SessionStart also fires for subagents; only root session transitions bind the terminal.
pub fn record(root: &Path, actor: &AgentIdentity, input: impl Read) -> Result<()> {
    let event: serde_json::Value = serde_json::from_reader(input.take(64 * 1024))?;
    if event["hook_event_name"] != "SessionStart"
        || !matches!(
            event["source"].as_str(),
            Some("startup" | "resume" | "clear")
        )
        || event.get("agent_id").is_some_and(|id| !id.is_null())
    {
        return Ok(());
    }
    let id: Uuid = event["session_id"]
        .as_str()
        .context("Missing native session ID")?
        .parse()?;
    save(root, actor, id)
}
pub fn arguments(root: &Path, actor: &AgentIdentity, resume: bool) -> Result<Vec<String>> {
    let saved = if resume { read(root, actor)? } else { None };
    let mut args = Vec::new();
    match actor.provider {
        Provider::Claude => {
            args.extend(["--settings".into(), serde_json::json!({"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"amux record-native-session","timeout":5}]}]}}).to_string()]);
            if let Some(id) = saved {
                args.extend(["--resume".into(), id.to_string()]);
            } else {
                let id = Uuid::new_v4();
                save(root, actor, id)?;
                args.extend(["--session-id".into(), id.to_string()]);
            }
        }
        Provider::Codex => {
            if let Some(id) = saved {
                args.extend(["resume".into(), id.to_string()]);
            }
            // CLI layer hooks merge with file layers; this never edits provider config.
            // Trust is intentionally left to Codex. Until trusted, no binding is recorded.
            args.extend(["-c".into(), "hooks.SessionStart=[{hooks=[{type=\"command\",command=\"amux record-native-session\",timeout=5}]}]".into()]);
            if resume && saved.is_none() {
                eprintln!(
                    "amux: no exact Codex conversation recorded for this terminal; starting fresh. Trust the Amux SessionStart hook in /hooks to enable exact resume."
                );
            }
        }
    }
    Ok(args)
}

#[cfg(test)]
mod tests {
    use super::*;
    fn actor(provider: Provider, n: u128) -> AgentIdentity {
        AgentIdentity {
            session_id: Uuid::from_u128(n),
            instance_id: Uuid::new_v4(),
            provider,
            os: "linux".into(),
            project_directory: "/p".into(),
            project_id: Uuid::new_v4(),
            agent_id: format!("{}-{n}", provider.command()),
            name: String::new(),
        }
    }
    #[test]
    fn two_personas_in_one_checkout_resume_their_own_conversations() {
        let root = tempfile::tempdir().unwrap();
        for provider in [Provider::Codex, Provider::Claude] {
            let a = actor(provider, 1);
            let b = actor(provider, 2);
            let first = Uuid::new_v4();
            let second = Uuid::new_v4();
            save(root.path(), &a, first).unwrap();
            save(root.path(), &b, second).unwrap();
            let aa = arguments(root.path(), &a, true).unwrap();
            let bb = arguments(root.path(), &b, true).unwrap();
            assert!(aa.contains(&first.to_string()) && !aa.contains(&second.to_string()));
            assert!(bb.contains(&second.to_string()) && !bb.contains(&first.to_string()));
            assert!(!aa.iter().any(|arg| arg == "--last" || arg == "--continue"));
        }
    }
    #[test]
    fn root_clear_updates_binding_but_fork_and_helper_do_not() {
        let root = tempfile::tempdir().unwrap();
        let a = actor(Provider::Codex, 1);
        let first = Uuid::new_v4();
        let second = Uuid::new_v4();
        save(root.path(), &a, first).unwrap();
        for event in [
            serde_json::json!({"hook_event_name":"SessionStart","source":"fork","session_id":second}),
            serde_json::json!({"hook_event_name":"SessionStart","source":"startup","session_id":second,"agent_id":"helper"}),
        ] {
            record(root.path(), &a, event.to_string().as_bytes()).unwrap();
            assert_eq!(read(root.path(), &a).unwrap(), Some(first));
        }
        record(root.path(), &a, serde_json::json!({"hook_event_name":"SessionStart","source":"clear","session_id":second}).to_string().as_bytes()).unwrap();
        assert_eq!(read(root.path(), &a).unwrap(), Some(second));
    }
}
