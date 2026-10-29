//! Canonical ordinary-task OS labels, shared by task storage and clients.
use anyhow::Result;

/// Rejected task labels are a client request error, not a state conflict.
#[derive(Debug)]
pub struct InvalidTaskLabels(pub String);
impl std::fmt::Display for InvalidTaskLabels {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}
impl std::error::Error for InvalidTaskLabels {}

/// Ordinary tasks restrict their executing agent OS with at most one canonical
/// `os:windows|os:linux|os:macos` label; no OS label means unrestricted.
pub fn required_os(labels: &[String]) -> Result<Option<&str>> {
    let mut required = None;
    for label in labels
        .iter()
        .filter(|l| l.get(..3).is_some_and(|p| p.eq_ignore_ascii_case("os:")))
    {
        let os = &label[3..];
        if !matches!(label.get(..3), Some("os:")) || !matches!(os, "windows" | "linux" | "macos") {
            return Err(InvalidTaskLabels(format!(
                "unsupported OS label '{label}': use os:windows, os:linux or os:macos"
            ))
            .into());
        }
        if required.replace(os).is_some() {
            return Err(InvalidTaskLabels("a task may carry at most one os: label".into()).into());
        }
    }
    Ok(required)
}
