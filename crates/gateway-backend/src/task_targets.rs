//! Ordinary agent availability uses the existing gateway authentication only.
use crate::{
    db, plugins::presence, routes::AppError, task_dispatch::Target, task_events, AppState,
};
use axum::{
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    Json,
};
use rusqlite::Connection;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

pub const TTL_MS: i64 = 120_000;

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
#[serde(tag = "mode", rename_all = "snake_case", deny_unknown_fields)]
pub enum Routing {
    #[default]
    Auto,
    Unassigned,
    Broadcast,
    Session {
        target: Target,
    },
}

pub fn initialize(conn: &Connection) -> anyhow::Result<()> {
    presence::initialize_directory(conn)
}

pub fn connected(conn: &Connection, project: &str, target: &Target) -> anyhow::Result<bool> {
    presence::target_connected(conn, project, target)
}

/// Display metadata from validated advertisements, without routing authority.
pub fn registered_host(
    conn: &Connection,
    project: &str,
    origin: &Value,
) -> anyhow::Result<Option<String>> {
    presence::registered_host(conn, project, origin)
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Heartbeat {
    pub display_name: Option<String>,
    pub hostname: Option<String>,
    pub canonical_remote: Option<String>,
    pub generation: String,
}

pub async fn heartbeat(
    State(state): State<AppState>,
    Path(project): Path<String>,
    headers: HeaderMap,
    Json(req): Json<Heartbeat>,
) -> Result<Json<Value>, AppError> {
    let origin = task_events::origin(&headers)?.ok_or_else(|| {
        AppError(
            StatusCode::BAD_REQUEST,
            "native agent origin required".into(),
        )
    })?;
    let generation = uuid::Uuid::parse_str(&req.generation)
        .map_err(|_| AppError(StatusCode::BAD_REQUEST, "generation must be a UUID".into()))?
        .to_string();
    let display_name = label(req.display_name)?;
    let hostname = label(req.hostname)?;
    let project = crate::projects::normalize_project_ident(&project);
    let conn = state.db.lock().unwrap();
    let remote = require_binding(&conn, &project, req.canonical_remote.as_deref())?;
    let seen = db::now_ms();
    presence::advertise(
        &conn,
        &project,
        &Target {
            origin: origin.clone(),
            generation: generation.clone(),
        },
        seen,
        remote.as_deref(),
        display_name.as_deref(),
        hostname.as_deref(),
    )?;
    Ok(Json(
        json!({"target":Target{origin,generation},"seen_at":seen,"expires_at":seen+TTL_MS,"canonical_remote":remote,"display_name":display_name,"hostname":hostname}),
    ))
}

pub async fn list(
    State(state): State<AppState>,
    Path(project): Path<String>,
) -> Result<Json<Value>, AppError> {
    let project = crate::projects::normalize_project_ident(&project);
    let conn = state.db.lock().unwrap();
    if db::get_project(&conn, &project)?.is_none() {
        return Err(AppError(StatusCode::NOT_FOUND, "project not found".into()));
    }
    let targets = presence::targets(&conn, &project)?;
    let remote = db::get_project(&conn, &project)?.and_then(|p| p.canonical_remote);
    Ok(Json(json!({"targets":targets,"canonical_remote":remote})))
}

/// Remote-backed projects require an exact repository match. Explicit ad-hoc
/// project bindings may omit the remote only while the registry has none.
pub fn require_binding(
    conn: &Connection,
    project: &str,
    supplied: Option<&str>,
) -> Result<Option<String>, AppError> {
    let project = db::get_project(conn, project)?
        .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "project not found".into()))?;
    let supplied = supplied.map(str::trim).filter(|s| !s.is_empty());
    let Some(remote) = project.canonical_remote else {
        if project.kind == db::PROJECT_KIND_ADHOC && supplied.is_none() {
            return Ok(None);
        }
        return Err(AppError(StatusCode::CONFLICT, "project binding requires a matching canonical repository or an explicit ad-hoc project without a remote".into()));
    };
    let actual = supplied.and_then(crate::projects::parse_remote);
    let expected = crate::projects::parse_remote(&remote);
    if actual.is_none()
        || expected.is_none()
        || actual.map(|r| r.canonical) != expected.map(|r| r.canonical)
    {
        return Err(AppError(
            StatusCode::CONFLICT,
            "canonical_remote must match the project repository".into(),
        ));
    }
    Ok(Some(remote))
}

fn label(value: Option<String>) -> Result<Option<String>, AppError> {
    let value = value
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty());
    if value
        .as_ref()
        .is_some_and(|s| s.len() > 128 || s.chars().any(char::is_control))
    {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "display_name and hostname must be at most 128 UTF-8 bytes with no control characters"
                .into(),
        ));
    }
    Ok(value)
}
