//! Explicit ordinary-task OS labels. Titles and prose never imply a platform.
use anyhow::{Result, bail, ensure};
use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, clap::ValueEnum)]
#[serde(rename_all = "lowercase")]
pub enum TaskOs {
    Windows,
    Linux,
    Macos,
}

impl TaskOs {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Windows => "windows",
            Self::Linux => "linux",
            Self::Macos => "macos",
        }
    }
}

pub fn required_os(task: &Value) -> Result<Option<TaskOs>> {
    let mut required = None;
    if task["labels"].is_null() {
        return Ok(None);
    }
    let labels = task["labels"]
        .as_array()
        .ok_or_else(|| anyhow::anyhow!("Invalid task labels"))?;
    for label in labels {
        let label = label
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("Invalid task label"))?;
        let Some(os) = label.strip_prefix("os:") else {
            ensure!(
                !label
                    .get(..3)
                    .is_some_and(|prefix| prefix.eq_ignore_ascii_case("os:")),
                "OS labels must use lowercase os:"
            );
            continue;
        };
        ensure!(required.is_none(), "Task has multiple OS requirements");
        required = Some(match os {
            "windows" => TaskOs::Windows,
            "linux" => TaskOs::Linux,
            "macos" => TaskOs::Macos,
            _ => bail!("Unsupported task OS requirement"),
        });
    }
    Ok(required)
}

pub fn compatible(required: Option<TaskOs>, os: Option<&str>) -> bool {
    required.is_none_or(|required| os == Some(required.as_str()))
}

pub fn matches(task: &Value, os: Option<&str>) -> bool {
    required_os(task).is_ok_and(|required| compatible(required, os))
}

pub fn labels_with_os(task: &Value, os: Option<TaskOs>) -> Result<Vec<String>> {
    let mut labels: Vec<String> = if task["labels"].is_null() {
        Vec::new()
    } else {
        serde_json::from_value(task["labels"].clone())?
    };
    labels.retain(|label| {
        !label
            .get(..3)
            .is_some_and(|prefix| prefix.eq_ignore_ascii_case("os:"))
    });
    if let Some(os) = os {
        labels.push(format!("os:{}", os.as_str()));
    }
    Ok(labels)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn explicit_requirements_fail_closed_and_retagging_preserves_other_labels() {
        assert_eq!(required_os(&json!({"title":"WINDOWS ONLY"})).unwrap(), None);
        let task = json!({"labels":["autoreview-finding","os:windows","project"]});
        assert!(matches(&task, Some("windows")));
        assert!(!matches(&task, Some("linux")));
        assert!(!matches(&task, None));
        assert_eq!(
            labels_with_os(&task, Some(TaskOs::Linux)).unwrap(),
            ["autoreview-finding", "project", "os:linux"]
        );
        assert_eq!(
            labels_with_os(&task, None).unwrap(),
            ["autoreview-finding", "project"]
        );
        for labels in [
            json!(["os:plan9"]),
            json!(["os:linux", "os:windows"]),
            json!(["os:Windows"]),
            json!(["OS:windows"]),
            json!([12]),
            json!("os:windows"),
        ] {
            assert!(!matches(&json!({"labels":labels}), Some("windows")));
        }
    }
}
