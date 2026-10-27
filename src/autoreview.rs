//! Review finding labels. Completion reviews themselves are queued by the
//! server's `review` plugin (docs/StateMachine.md).

pub const GUIDELINES: &str = include_str!("../docs/general_guidelines.md");

/// A label is descriptive. Only an explicit user-review state holds work.
pub fn held_finding(task: &serde_json::Value) -> bool {
    task["labels"]
        .as_array()
        .is_some_and(|labels| labels.iter().any(|l| l == "autoreview-finding"))
        && task["status"] == "user_review"
}
