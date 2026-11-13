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
