//! A destination completion requests source inspection; only source confirmation
//! completes the delegation. All effects share the task mutation transaction.
use crate::{db, templates};
use anyhow::Result;
use rusqlite::{params, Connection};

pub fn ready(conn: &Connection, source: &str) -> Result<bool> {
    Ok(conn.query_row(
        "SELECT EXISTS(SELECT 1 FROM task_delegations d JOIN tasks t ON t.id=d.target_task_id
         AND t.project_ident=d.target_project_ident WHERE d.source_task_id=?1 AND t.status='done')",
        [source],
        |row| row.get(0),
    )?)
}

pub fn completed(conn: &Connection, task: &db::Task) -> Result<()> {
    if let Some(link) = db::get_delegation_by_target(conn, &task.project_ident, &task.id)? {
        let detail = db::get_task_detail(conn, &task.project_ident, &task.id)?
            .ok_or_else(|| anyhow::anyhow!("target delegated task missing"))?;
        let comments = detail
            .comments
            .iter()
            .filter(|c| c.author_type != "system")
            .map(|c| {
                templates::render(
                    templates::DELEGATED_COMPLETED_COMMENT,
                    &[("author", &c.author), ("content", &c.content)],
                )
            })
            .collect::<Vec<_>>()
            .join("\n");
        let summary = db::cap_comment(&templates::render(
            templates::DELEGATED_COMPLETED,
            &[
                ("target_project", &task.project_ident),
                ("title", &task.title),
                ("description", task.description.as_deref().unwrap_or("")),
                ("specification", task.details.as_deref().unwrap_or("")),
                (
                    "comments",
                    if comments.is_empty() {
                        "(none)"
                    } else {
                        &comments
                    },
                ),
            ],
        ))
        .into_owned();
        db::insert_comment(
            conn,
            &link.source_task_id,
            "agent-gateway",
            "system",
            &summary,
        )?;
        let source = db::get_task_detail(conn, &link.source_project_ident, &link.source_task_id)?
            .ok_or_else(|| anyhow::anyhow!("source delegated task missing"))?
            .task;
        if source.status == "todo" {
            db::update_task_mirrored(
                conn,
                &link.source_project_ident,
                &source.id,
                &db::TaskUpdate {
                    status: Some("todo"),
                    owner_agent_id: Some(None),
                    ..Default::default()
                },
            )?;
            // The old delegator may no longer exist. Existing assignment selects
            // a live agent respecting source project, OS, goal and availability.
            conn.execute(
                r#"UPDATE tasks SET routing_json='{"mode":"auto"}' WHERE id=?1"#,
                [&source.id],
            )?;
        }
        // Destination completion is not delegation acceptance. Do not set
        // completed_at or auto-close the source here.
    }
    if let Some(link) = db::get_delegation_by_source(conn, &task.project_ident, &task.id)? {
        if !ready(conn, &task.id)? {
            // Closing a tracker is always allowed. An unfinished target is not
            // accepted or closed by that decision.
            return Ok(());
        }
        let message = templates::render(
            templates::DELEGATED_CONFIRMED,
            &[
                ("source_project", &task.project_ident),
                ("source_task_id", &task.id),
                ("target_task_id", &link.target_task_id),
            ],
        );
        db::insert_comment(
            conn,
            &link.target_task_id,
            "agent-gateway",
            "system",
            &message,
        )?;
        // Completion has no new project-wide work; the linked comment uses the
        // existing owner/requester notice path rather than broadcasting a task.
        conn.execute(
            "UPDATE task_delegations SET completed_at=?2 WHERE id=?1",
            params![link.id, db::now_ms()],
        )?;
    }
    Ok(())
}

pub fn inspecting(conn: &Connection, project: &str, source: &str) -> Result<()> {
    let Some(link) = db::get_delegation_by_source(conn, project, source)? else {
        return Ok(());
    };
    let message = templates::render(
        templates::DELEGATED_INSPECTING,
        &[
            ("source_project", &link.source_project_ident),
            ("source_task_id", source),
            ("target_task_id", &link.target_task_id),
        ],
    );
    db::insert_comment(
        conn,
        &link.target_task_id,
        "agent-gateway",
        "system",
        &message,
    )?;
    Ok(())
}
