//! The terminal notification contract shared by plugins that message agents.
//! The client formats and safely delivers `agent.notify`, then reports the
//! outcome as `agent.notify_result` correlated with it.
use crate::router::EventDefinition;
use serde_json::Value;

pub const NOTIFY: &str = "agent.notify";
pub const RESULT: &str = "agent.notify_result";

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
