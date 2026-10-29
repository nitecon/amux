//! The terminal notification contract shared by plugins that message agents.
//! The client formats and safely delivers `agent.notify`, then reports the
//! outcome as `agent.notify_result` correlated with it.
use crate::{
    router::{EventDefinition, StepContext},
    task_targets::registered_host,
};
use anyhow::Result;
use rusqlite::Connection;
use serde_json::{json, Value};

pub const NOTIFY: &str = "agent.notify";
pub const RESULT: &str = "agent.notify_result";

/// Snapshot hosts alongside the notification so rendering needs no live lookup.
pub(crate) fn emit(conn: &Connection, ctx: &StepContext, mut payload: Value) -> Result<uuid::Uuid> {
    let project = payload["project"].as_str().unwrap_or_default();
    let owner_host = registered_host(conn, project, &payload["event"]["task"]["owner_origin"])?;
    let sender_host = if payload["event"]["origin"].is_null()
        && (payload["mode"] == "assigned" || payload["event"]["sender_name"] == "amux")
    {
        payload["event"]["sender_name"] = json!("amux");
        sysinfo::System::host_name()
    } else {
        registered_host(conn, project, &payload["event"]["origin"])?
    };
    payload["event"]["sender_hostname"] = json!(sender_host);
    payload["event"]["owner_hostname"] = json!(owner_host);
    ctx.emit_in(conn, NOTIFY, 1, payload)
}

pub fn events() -> Vec<EventDefinition> {
    vec![
        EventDefinition {
            id: NOTIFY,
            version: 1,
            remote_emit: false,
            validate: notify,
        },
        EventDefinition {
            id: RESULT,
            version: 1,
            remote_emit: true,
            validate: result,
        },
    ]
}

/// `{"target": session, "project", "mode": "assigned"|"recipient",
///   "event": {"kind","project_ident","created_at","task","comment","origin"},
///   "heading": string|null, "instructions": string|null, "require_idle": bool}`
///
/// `event` uses the task-event shape terminals already render. `assigned`
/// tells the agent the task is already claimed for it.
fn notify(payload: &Value) -> std::result::Result<(), String> {
    if !payload["target"].is_string() || !payload["project"].is_string() {
        return Err("target and project must be strings".into());
    }
    if !matches!(payload["mode"].as_str(), Some("assigned" | "recipient")) {
        return Err("mode must be assigned or recipient".into());
    }
    if !payload["event"]["kind"].is_string() || !payload["event"]["task"].is_object() {
        return Err("event needs a kind and a task".into());
    }
    if payload.get("receiver").is_some_and(|receiver| {
        receiver["session_id"] != payload["target"]
            || !receiver["instance_id"].is_string()
            || receiver["project"] != payload["project"]
    }) {
        return Err("receiver must identify the target execution and project".into());
    }
    if !payload["require_idle"].is_boolean() {
        return Err("require_idle must be a boolean".into());
    }
    Ok(())
}

/// `{"target": session, "status": "delivered"|"skipped"|"failed", "reason"}`
fn result(payload: &Value) -> std::result::Result<(), String> {
    if !payload["target"].is_string() {
        return Err("target must be a string".into());
    }
    if !matches!(
        payload["status"].as_str(),
        Some("delivered" | "skipped" | "failed")
    ) {
        return Err("status must be delivered, skipped or failed".into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{db, plugins, router};
    use rusqlite::params;

    #[tokio::test]
    async fn notifications_snapshot_registered_sender_and_owner_hosts_without_agent_metadata() {
        let database = db::open(":memory:").unwrap();
        let remote = plugins::remote::Remote::default();
        let (bus, _router) = router::start(
            database.clone(),
            plugins::events(),
            plugins::plugins(&remote),
        )
        .unwrap();
        let ctx = router::test_context(&bus, "notifier");
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)", []).unwrap();
        for (session, instance, host, seen) in [
            ("sender", "old", "SenderMachine", 1),
            ("sender", "new", "ReplacementMachine", 2),
            ("owner", "live", "OwnerMachine", 3),
        ] {
            conn.execute("INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,canonical_remote,hostname) VALUES('p',?1,?2,'{}','g',?3,'',?4)", params![instance,session,seen,host]).unwrap();
        }
        // A cross-project caller and an older sender execution must retain their
        // own machine, rather than the recipient or a replacement's machine.
        for kind in ["task_created", "task_commented", "task_completed"] {
            let uid = emit(&conn, &ctx, json!({"target":"owner", "project":"other-project", "mode":"recipient", "require_idle":false,
                "event":{"kind":kind,"origin":{"session_id":"sender","instance_id":"old"},
                    "task":{"hostname":"CreationMachine","owner_origin":{"session_id":"owner","instance_id":"live"}}}})).unwrap();
            let raw: String = conn
                .query_row(
                    "SELECT payload FROM router_events WHERE uid=?1",
                    [uid.to_string()],
                    |row| row.get(0),
                )
                .unwrap();
            let payload: Value = serde_json::from_str(&raw).unwrap();
            assert_eq!(payload["event"]["sender_hostname"], "SenderMachine");
            assert_eq!(payload["event"]["owner_hostname"], "OwnerMachine");
            assert_eq!(payload["event"]["task"]["hostname"], "CreationMachine");
        }
        for mode in ["assigned", "recipient"] {
            let uid = emit(
                &conn,
                &ctx,
                json!({"target":"owner", "project":"p", "mode":mode, "require_idle":true,
                "event":{"kind":"task_created","origin":null,"sender_name":if mode == "recipient" {json!("amux")} else {Value::Null},"task":{}}}),
            )
            .unwrap();
            let raw: String = conn
                .query_row(
                    "SELECT payload FROM router_events WHERE uid=?1",
                    [uid.to_string()],
                    |row| row.get(0),
                )
                .unwrap();
            let payload: Value = serde_json::from_str(&raw).unwrap();
            assert_eq!(payload["event"]["sender_name"], "amux");
            assert_eq!(
                payload["event"]["sender_hostname"],
                json!(sysinfo::System::host_name())
            );
            assert!(
                payload["event"]["owner_hostname"].is_null(),
                "unknown owners cannot inherit a sender host"
            );
        }
    }
}
