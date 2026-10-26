//! Markdown rendering of gateway task replies, so agents read them directly
//! instead of parsing JSON. `amux tasks --json` keeps the raw reply.
use serde_json::Value;
use std::fmt::Write;
use std::time::{Duration, UNIX_EPOCH};

pub fn render(value: &Value) -> String {
    match value {
        Value::Array(items) if items.iter().all(is_task) => list(items),
        value if is_task(value) => task(value),
        value if value.get("task_id").is_some() && value.get("content").is_some() => format!(
            "Comment {} added to task {}.\n",
            text(&value["id"]),
            text(&value["task_id"])
        ),
        // Other replies (agents, cancellation, bind) have no stable task shape.
        value => format!(
            "```json\n{}\n```\n",
            serde_json::to_string_pretty(value).unwrap_or_default()
        ),
    }
}

fn is_task(value: &Value) -> bool {
    value.get("id").is_some() && value.get("title").is_some() && value.get("status").is_some()
}

fn list(tasks: &[Value]) -> String {
    if tasks.is_empty() {
        return "No open tasks.\n".into();
    }
    let mut out = String::new();
    for task in tasks {
        let _ = write!(
            out,
            "- [{}] {} — {}",
            text(&task["status"]),
            text(&task["id"]),
            text(&task["title"])
        );
        if let Some(owner) = owner(task) {
            let _ = write!(out, " · owner {owner}");
        }
        if let Some(labels) = labels(task) {
            let _ = write!(out, " · labels {labels}");
        }
        if let Some(count) = task["comment_count"].as_u64().filter(|c| *c > 0) {
            let _ = write!(out, " · {count} comments");
        }
        out.push('\n');
    }
    out
}

fn task(task: &Value) -> String {
    let mut out = format!("# {}\n\n", text(&task["title"]));
    let mut field = |name: &str, value: Option<String>| {
        if let Some(value) = value.filter(|v| !v.is_empty()) {
            let _ = writeln!(out, "- **{name}:** {value}");
        }
    };
    field("ID", Some(text(&task["id"])));
    field("Status", Some(text(&task["status"])));
    field("Owner", owner(task));
    field("Reporter", Some(text(&task["reporter"])));
    field("Routing", Some(text(&task["routing"]["mode"])));
    if let Ok(requester) =
        serde_json::from_value::<crate::task_routing::SessionTarget>(task["requester"].clone())
    {
        field(
            "Result recipient",
            Some(format!(
                "{} ({}/{}) · execution {}",
                requester.origin.session_id,
                requester.origin.provider.command(),
                requester.origin.os,
                requester.generation
            )),
        );
    }
    field("Labels", labels(task));
    field("Goal", Some(text(&task["goal"])));
    field("Created", time(&task["created_at"]));
    field("Updated", time(&task["updated_at"]));
    field("Done", time(&task["done_at"]));
    let delegated = text(&task["delegated_to_task_id"]);
    field(
        "Delegated to",
        (!delegated.is_empty()).then(|| {
            format!(
                "{} task {delegated}",
                text(&task["delegated_to_project_ident"])
            )
        }),
    );
    let specification = text(&task["specification"]);
    for (heading, body) in [
        ("Description", text(&task["description"])),
        ("Specification", specification.clone()),
        // Details usually repeat the specification; show them only when they differ.
        (
            "Details",
            Some(text(&task["details"]))
                .filter(|d| *d != specification)
                .unwrap_or_default(),
        ),
    ] {
        if !body.trim().is_empty() {
            let _ = write!(out, "\n## {heading}\n\n{}\n", body.trim_end());
        }
    }
    if let Some(comments) = task["comments"].as_array() {
        let _ = write!(out, "\n## Comments ({})\n", comments.len());
        for (index, comment) in comments.iter().enumerate() {
            let _ = write!(
                out,
                "\n### {}. {}{}\n\n{}\n",
                index + 1,
                text(&comment["author"]),
                time(&comment["created_at"])
                    .map(|t| format!(" — {t}"))
                    .unwrap_or_default(),
                text(&comment["content"]).trim_end()
            );
        }
    }
    out
}

fn owner(task: &Value) -> Option<String> {
    let origin = &task["owner_origin"];
    let session = text(&task["owner_agent_id"]);
    let session = if session.is_empty() {
        text(&origin["session_id"])
    } else {
        session
    };
    (!session.is_empty()).then(|| {
        let platform = [text(&origin["provider"]), text(&origin["os"])]
            .into_iter()
            .filter(|p| !p.is_empty())
            .collect::<Vec<_>>()
            .join("/");
        if platform.is_empty() {
            session
        } else {
            format!("{session} ({platform})")
        }
    })
}

fn labels(task: &Value) -> Option<String> {
    let labels: Vec<_> = task["labels"].as_array()?.iter().map(text).collect();
    (!labels.is_empty()).then(|| labels.join(", "))
}

fn time(value: &Value) -> Option<String> {
    let millis = value.as_u64()?;
    Some(httpdate::fmt_http_date(
        UNIX_EPOCH + Duration::from_millis(millis),
    ))
}

fn text(value: &Value) -> String {
    match value {
        Value::Null => String::new(),
        Value::String(s) => s.clone(),
        other => other.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn task_detail_and_list_render_every_comment_and_skip_empty_fields() {
        let task = json!({
            "id": "t1", "title": "Fix output", "status": "in_progress",
            "owner_agent_id": "s1", "owner_origin": {"provider": "claude", "os": "linux"},
            "reporter": "user", "routing": {"mode": "session"}, "labels": ["os:linux"],
            "created_at": 0, "specification": "Do it.", "details": "Do it.", "description": null,
            "comments": [
                {"author": "A", "content": "first", "created_at": 0},
                {"author": "B", "content": "second\n", "created_at": 1000}
            ]
        });
        let out = render(&task);
        assert!(out.starts_with("# Fix output\n"));
        assert!(out.contains("- **Owner:** s1 (claude/linux)\n"));
        assert!(out.contains("- **Created:** Thu, 01 Jan 1970 00:00:00 GMT\n"));
        assert!(out.contains("## Specification\n\nDo it.\n"));
        assert!(
            !out.contains("## Details") && !out.contains("Description") && !out.contains("Goal")
        );
        assert!(
            out.contains("## Comments (2)\n\n### 1. A — Thu, 01 Jan 1970 00:00:00 GMT\n\nfirst\n")
        );
        assert!(out.ends_with("### 2. B — Thu, 01 Jan 1970 00:00:01 GMT\n\nsecond\n"));

        let listed = render(
            &json!([{"id": "t1", "title": "Fix output", "status": "todo", "comment_count": 3}]),
        );
        assert_eq!(listed, "- [todo] t1 — Fix output · 3 comments\n");
        assert_eq!(render(&json!([])), "No open tasks.\n");
        assert!(render(&json!({"agents": []})).starts_with("```json\n"));
    }
}
