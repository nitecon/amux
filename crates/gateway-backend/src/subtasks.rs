use crate::{db, routes::AppError, AppState};
use axum::{
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    Json,
};
use rusqlite::{params, Connection};
use serde::Deserialize;

/// Parent lookup is scoped to the submitting project. Callers validate any root exception.
pub fn parent_for_creation(
    conn: &Connection,
    project: &str,
    parent_id: Option<&str>,
    require_parent: bool,
) -> Result<Option<db::Task>, AppError> {
    let Some(id) = parent_id else {
        return if require_parent {
            Err(AppError(StatusCode::BAD_REQUEST,
                "agent task creation requires parent_task_id, or root_reason=user-directed|principle with an explanatory specification; read a fitting parent first".into()))
        } else {
            Ok(None)
        };
    };
    db::get_task_detail_recent(conn, project, id, Some(0))?
        .map(|detail| Some(detail.task))
        .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "parent task not found in this project; choose a fitting parent or ask the user to create a top-level task".into()))
}

/// Called within the same mutation as the child's INSERT, before publication.
pub fn attach(conn: &Connection, parent: &db::Task, child: &mut db::Task) -> anyhow::Result<()> {
    conn.execute(
        "INSERT INTO task_subtasks(parent_id,child_id) VALUES (?1,?2)",
        params![parent.id, child.id],
    )?;
    child.parent = Some(db::TaskParent {
        id: parent.id.clone(),
        project_ident: parent.project_ident.clone(),
        title: parent.title.clone(),
    });
    // The INSERT trigger runs before the link exists. Complete its snapshot
    // in this transaction, before any consumer can observe the created event.
    conn.execute(
        "UPDATE task_events SET task_json=json_set(task_json,'$.parent',json(?1))
         WHERE id=(SELECT id FROM task_events WHERE kind='task_created'
                   AND json_extract(task_json,'$.id')=?2 ORDER BY id DESC LIMIT 1)",
        params![serde_json::to_string(&child.parent)?, child.id],
    )?;
    conn.execute(
        "UPDATE router_task_outbox SET task_json=json_set(task_json,'$.parent',json(?1))
         WHERE id=(SELECT id FROM router_task_outbox WHERE kind='created'
                   AND json_extract(task_json,'$.id')=?2 ORDER BY id DESC LIMIT 1)",
        params![serde_json::to_string(&child.parent)?, child.id],
    )?;
    Ok(())
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CreateSubtask {
    pub title: String,
    pub description: Option<String>,
    pub specification: Option<String>,
    pub target_project_ident: Option<String>,
    #[serde(default)]
    pub labels: Vec<String>,
}

pub fn list(conn: &Connection, parent_id: &str) -> anyhow::Result<Vec<db::Task>> {
    let mut statement = conn.prepare("SELECT t.id,t.project_ident FROM task_subtasks s JOIN tasks t ON t.id=s.child_id WHERE s.parent_id=?1 ORDER BY t.created_at,t.id")?;
    let refs = statement
        .query_map([parent_id], |r| {
            Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?))
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    refs.into_iter()
        .map(|(id, project)| db::get_task_detail(conn, &project, &id).map(|t| t.unwrap().task))
        .collect()
}

pub async fn get(
    State(state): State<AppState>,
    Path((project, id)): Path<(String, String)>,
) -> Result<Json<Vec<db::Task>>, AppError> {
    let conn = state.db.lock().unwrap();
    if db::get_task_detail(&conn, &project, &id)?.is_none() {
        return Err(AppError(
            StatusCode::NOT_FOUND,
            "parent task not found".into(),
        ));
    }
    Ok(Json(list(&conn, &id)?))
}

pub async fn create(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((project, id)): Path<(String, String)>,
    Json(request): Json<CreateSubtask>,
) -> Result<Json<db::Task>, AppError> {
    let origin = crate::routes::task_creation_origin(&headers)?;
    let conn = state.db.lock().unwrap();
    let parent = db::get_task_detail_recent(&conn, &project, &id, Some(0))?
        .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "parent task not found".into()))?;
    if request.title.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "title must be non-empty".into(),
        ));
    }
    let target = request.target_project_ident.as_deref().unwrap_or(&project);
    let target = db::get_project(&conn, target)?
        .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "target project not found".into()))?;
    if target.archived_at.is_some() {
        return Err(AppError(
            StatusCode::CONFLICT,
            "target project is archived".into(),
        ));
    }
    let reporter = crate::routes::resolve_identity(None, &headers);
    let task = crate::task_events::mutation(&conn, origin.as_ref(), |tx| {
        let mut task = db::insert_task_with_goal(
            tx,
            &target.ident,
            request.title.trim(),
            request.description.as_deref(),
            request.specification.as_deref(),
            &request.labels,
            None,
            &reporter,
            if target.ident == parent.task.project_ident {
                parent.task.goal.as_deref()
            } else {
                None
            },
        )?;
        attach(tx, &parent.task, &mut task)?;
        Ok(task)
    })?;
    Ok(Json(task))
}
