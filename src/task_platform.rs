//! Explicit ordinary-task OS labels. Titles and prose never imply a platform.
use anyhow::{Context, Result};
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
    if task["labels"].is_null() {
        return Ok(None);
    }
    let labels = task["labels"]
        .as_array()
        .context("Invalid task labels")?
        .iter()
        .map(|label| {
            label
                .as_str()
                .context("Invalid task label")
                .map(String::from)
        })
        .collect::<Result<Vec<_>>>()?;
    Ok(
        match amux_gateway_backend::task_platform::required_os(&labels)? {
            Some("windows") => Some(TaskOs::Windows),
            Some("linux") => Some(TaskOs::Linux),
            Some("macos") => Some(TaskOs::Macos),
            None => None,
            Some(_) => unreachable!("shared task OS validator returned a noncanonical OS"),
        },
    )
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

    #[test]
    fn task_store_client_assignment_and_notifier_agree_on_platform_eligibility() {
        use amux_gateway_backend::{db, plugins, task_platform};
        let agent =
            json!({"session_id":"session", "instance_id":"instance", "os":"linux", "goal":null});
        for (labels, expected) in [
            (Value::Null, true),
            (json!([]), true),
            (json!(["ordinary", "é", "🚀"]), true),
            (json!(["os:linux"]), true),
            (json!(["os:windows"]), false),
            (json!(["os:Linux"]), false),
            (json!(["OS:linux"]), false),
            (json!(["os:plan9"]), false),
            (json!(["os:linux", "os:windows"]), false),
            (json!(["os:linux", "os:linux"]), false),
            (json!([12]), false),
            (json!(["os:linux", 12]), false),
            (json!("os:linux"), false),
        ] {
            let mut task = json!({"labels":labels, "status":"todo", "kind":"task", "goal":null,
                "owner_origin":null, "owner_agent_id":null, "routing":{"mode":"auto"}});
            assert_eq!(matches(&task, Some("linux")), expected, "client: {labels}");
            assert_eq!(
                plugins::assignment::eligible(&task, &agent),
                expected,
                "assignment: {labels}"
            );
            task["status"] = json!("in_progress");
            task["owner_origin"] = json!({"session_id":"session", "instance_id":"instance"});
            let payload = json!({"task":task, "actor":null});
            assert_eq!(
                plugins::notifier::recipient("task_commented", &payload, &agent),
                expected,
                "notifier: {labels}"
            );
            // Storage has a typed label-list boundary; test every representable
            // input against the same routing decision and legacy error alias.
            let typed: Result<Vec<String>, _> = if labels.is_null() {
                Ok(Vec::new())
            } else {
                serde_json::from_value(labels.clone())
            };
            if let Ok(typed) = typed {
                let required = task_platform::required_os(&typed);
                if let Err(error) = &required {
                    assert!(error.downcast_ref::<db::InvalidTaskLabels>().is_some());
                }
                assert_eq!(
                    required.is_ok_and(|os| os.is_none_or(|os| os == "linux")),
                    expected,
                    "storage: {labels}"
                );
            }
        }
    }
}
