//! File mutation confirmation belongs to the transition, not task creation.
use crate::task_dispatch::Conflict;
use anyhow::Result;
use serde_json::Value;

pub const QUESTION: &str = "Did this task mutate any files? Repeat the transition with changeset: [\"path/to/file\"] listing every modified, added or deleted file, or changeset: null if no files were mutated. The task has not moved.";

pub fn validate(value: &Value) -> Result<()> {
    if value.is_null() {
        return Ok(());
    }
    let valid = value.as_array().is_some_and(|paths| {
        !paths.is_empty()
            && paths.len() <= 1024
            && paths.iter().all(|p| {
                p.as_str()
                    .is_some_and(|p| !p.trim().is_empty() && p.len() <= 4096 && !p.contains('\0'))
            })
    });
    if !valid || value.to_string().len() > 65536 {
        return Err(Conflict(
            "changeset must be null or a non-empty array of file paths (at most 64 KiB)",
        )
        .into());
    }
    Ok(())
}

/// Record a confirmation without losing files confirmed earlier for the task:
/// array paths are added to the stored list and null keeps it. Returns whether
/// the task has recorded file mutations.
pub fn record(
    conn: &rusqlite::Connection,
    task_id: &str,
    prior: Option<&[String]>,
    value: &Value,
) -> Result<bool> {
    let mut files = prior.unwrap_or_default().to_vec();
    for path in value
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
    {
        if !files.iter().any(|file| file == path) {
            files.push(path.to_owned());
        }
    }
    let stored = (!files.is_empty()).then(|| serde_json::json!(files));
    conn.execute(
        "UPDATE tasks SET changeset_json=?1 WHERE id=?2",
        rusqlite::params![stored.unwrap_or(Value::Null).to_string(), task_id],
    )?;
    Ok(!files.is_empty())
}

/// The requested destination is durable, but the task remains in its old lane
/// until its exact owning execution supplies confirmation.
pub fn initialize(conn: &rusqlite::Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS task_confirmations (
        task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
        token TEXT NOT NULL, origin_json TEXT NOT NULL, snapshot_json TEXT NOT NULL,
        request_json TEXT NOT NULL, mutated INTEGER NOT NULL DEFAULT 0
    )",
    )?;
    Ok(())
}

#[derive(Debug)]
pub struct GuidedQuestion(pub String);
impl std::fmt::Display for GuidedQuestion {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&question(&self.0))
    }
}
impl std::error::Error for GuidedQuestion {}

pub fn question(id: &str) -> String {
    format!("Did this task mutate any files?\nTask: {id}\nThe task has not moved. If files were mutated, run `amux set-task {id} software` (MCP `task_confirm_mutation`), then follow the changeset instruction. If no files were mutated, run `amux set-changeset {id} null` (MCP `task_set_changeset` with changeset null).")
}

pub fn remember(
    conn: &rusqlite::Connection,
    project: &str,
    id: &str,
    origin: &crate::task_events::AgentOrigin,
    request: &str,
) -> Result<()> {
    let task = owned(conn, project, id, Some(origin))?;
    conn.execute("INSERT OR REPLACE INTO task_confirmations(task_id,token,origin_json,snapshot_json,request_json) VALUES(?1,?2,?3,?4,?5)",
        rusqlite::params![id, uuid::Uuid::now_v7().to_string(), serde_json::to_string(origin)?, serde_json::to_string(&task)?, request])?;
    Ok(())
}

fn owned(
    conn: &rusqlite::Connection,
    project: &str,
    id: &str,
    origin: Option<&crate::task_events::AgentOrigin>,
) -> Result<crate::db::Task> {
    let task = crate::db::get_task_detail(conn, project, id)?
        .ok_or(Conflict("task not found"))?
        .task;
    if origin.is_none() || task.owner_origin.as_ref() != origin {
        return Err(crate::task_events::OwnershipConflict.into());
    }
    if crate::task_dispatch::managed(conn, id)?
        || !matches!(task.status.as_str(), "todo" | "in_progress")
    {
        return Err(Conflict("confirmation requires owned ordinary active work").into());
    }
    Ok(task)
}

pub struct Pending {
    pub token: String,
    pub request: String,
    pub mutated: bool,
}

pub fn pending(
    conn: &rusqlite::Connection,
    project: &str,
    id: &str,
    origin: Option<&crate::task_events::AgentOrigin>,
) -> Result<Pending> {
    use rusqlite::OptionalExtension;
    let task = owned(conn, project, id, origin)?;
    let row = conn.query_row("SELECT token,origin_json,snapshot_json,request_json,mutated FROM task_confirmations WHERE task_id=?1", [id], |r| {
        Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?, r.get::<_, String>(2)?, r.get::<_, String>(3)?, r.get::<_, bool>(4)?))
    }).optional()?.ok_or(Conflict("no pending confirmation; repeat the requested task transition"))?;
    if Some(serde_json::from_str::<crate::task_events::AgentOrigin>(
        &row.1,
    )?) != origin.cloned()
        || row.2 != serde_json::to_string(&task)?
    {
        return Err(
            Conflict("task changed since the question; repeat the requested transition").into(),
        );
    }
    Ok(Pending {
        token: row.0,
        request: row.3,
        mutated: row.4,
    })
}

#[derive(serde::Deserialize)]
pub struct ConfirmationRequest {
    pub task_type: Option<String>,
    #[serde(default, deserialize_with = "crate::routes::present_nullable_value")]
    pub changeset: Option<Value>,
}

pub async fn confirm_handler(
    axum::extract::State(state): axum::extract::State<crate::AppState>,
    headers: axum::http::HeaderMap,
    axum::extract::Path((project, id)): axum::extract::Path<(String, String)>,
    axum::Json(req): axum::Json<ConfirmationRequest>,
) -> crate::routes::Result<axum::Json<Value>> {
    let origin = crate::task_events::origin(&headers)?;
    if req.task_type.is_some() == req.changeset.is_some() {
        return Err(Conflict("supply mutation confirmation or a changeset, not both").into());
    }
    if req
        .task_type
        .as_deref()
        .is_some_and(|kind| kind != "software")
    {
        return Err(Conflict(
            "set-task confirms software mutation only; for no changes use set-changeset null",
        )
        .into());
    }
    if let Some(value) = &req.changeset {
        validate(value)?;
    }
    let db = state.db.clone();
    let saved_project = project.clone();
    let saved_id = id.clone();
    let confirming_mutation = req.task_type.is_some();
    let pending = tokio::task::spawn_blocking(move || -> Result<Pending> {
        let conn = db.lock().unwrap();
        crate::task_events::mutation(&conn, origin.as_ref(), |conn| {
            let pending = pending(conn, &saved_project, &saved_id, origin.as_ref())?;
            if confirming_mutation {
                let before = owned(conn, &saved_project, &saved_id, origin.as_ref())?;
                // Generated review classification is managed by the workflow.
                let update = crate::db::TaskUpdate {
                    task_type: (!before.labels.iter().any(|l| l == "task-review"))
                        .then_some("software"),
                    ..Default::default()
                };
                let task = crate::db::update_task(
                    conn,
                    &saved_project,
                    &saved_id,
                    &update,
                    origin.as_ref().map(|o| o.session_id.as_str()),
                )?
                .ok_or(Conflict("task not found"))?;
                conn.execute(
                    "UPDATE task_confirmations SET mutated=1,snapshot_json=?1 WHERE task_id=?2",
                    rusqlite::params![serde_json::to_string(&task)?, saved_id],
                )?;
            }
            Ok(pending)
        })
    })
    .await??;
    if confirming_mutation {
        return Ok(axum::Json(
            serde_json::json!({"task_id":id, "message":format!("Task mutation confirmed; the task has not moved. Submit every modified, added or deleted path using `amux set-changeset {id} '[\"path/to/file\"]'` (MCP `task_set_changeset`)."), "next_tool":"task_set_changeset"}),
        ));
    }
    let mut update: crate::routes::UpdateTaskRequest = serde_json::from_str(&pending.request)?;
    update.changeset = req.changeset;
    update.confirmation_token = Some(pending.token);
    let task = crate::routes::update_task_handler(
        axum::extract::State(state),
        headers,
        axum::extract::Path((project, id)),
        axum::Json(update),
    )
    .await?
    .0;
    Ok(axum::Json(serde_json::to_value(task)?))
}
