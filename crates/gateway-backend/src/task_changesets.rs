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
