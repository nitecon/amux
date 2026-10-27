//! Built-in event sources and plugins registered with the event router.
pub mod assignment;
pub mod clock;
pub mod notifier;
pub mod notify;
pub mod presence;
pub mod reclaim;
pub mod reminder;
pub mod remote;
pub mod review;
pub mod tasks;

use crate::{
    db,
    router::{Event, EventDefinition, Plugin, Step, StepContext, StepOutcome},
    routes::{require_admin, AppError},
    AppState,
};
use async_trait::async_trait;
use axum::{
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    Json,
};
use rusqlite::Connection;
use serde_json::{json, Value};

pub fn events() -> Vec<EventDefinition> {
    let mut events = vec![clock::definition()];
    events.extend(tasks::events());
    events.extend(remote::events());
    events.extend(presence::events());
    events.extend(notify::events());
    events
}

/// Events remote consumers may subscribe to.
fn forwarded() -> Vec<&'static str> {
    let mut forwarded: Vec<_> = tasks::events().iter().map(|event| event.id).collect();
    forwarded.extend(presence::events().iter().map(|event| event.id));
    forwarded.push(notify::NOTIFY);
    forwarded.push(crate::router::RETRYING);
    forwarded
}

pub fn plugins(remote: &remote::Remote) -> Vec<Box<dyn Plugin>> {
    vec![
        remote.plugin(forwarded()),
        Box::new(presence::Presence),
        Box::new(assignment::Assignment),
        Box::new(notifier::Notifier),
        Box::new(reminder::Reminder),
        Box::new(review::Review),
        Box::new(reclaim::Reclaim),
    ]
}

/// Plugins a user may switch off with the `plugin.<id>.enabled` setting.
pub const SWITCHABLE: [&str; 5] = [
    assignment::PLUGIN,
    notifier::PLUGIN,
    reminder::PLUGIN,
    review::PLUGIN,
    reclaim::PLUGIN,
];

fn setting(id: &str) -> String {
    format!("plugin.{id}.enabled")
}

pub fn enabled(conn: &Connection, id: &str) -> anyhow::Result<bool> {
    Ok(db::get_setting(conn, &setting(id))?.as_deref() != Some("false"))
}

/// Pre-exec step that stops a plugin's run while it is switched off.
pub struct Enabled(pub &'static str);

#[async_trait]
impl Step for Enabled {
    async fn run(&self, _event: &Event, ctx: &StepContext) -> StepOutcome {
        match enabled(&ctx.db().lock().unwrap(), self.0) {
            Ok(true) => StepOutcome::Continue,
            Ok(false) => StepOutcome::Stop,
            Err(error) => StepOutcome::Fail(error.to_string()),
        }
    }
}

/// `GET /v1/plugins`: the switchable plugins and whether each is enabled.
pub async fn list(
    State(state): State<AppState>,
    headers: HeaderMap,
) -> Result<Json<Value>, AppError> {
    require_admin(&headers)?;
    let conn = state.db.lock().unwrap();
    let plugins = SWITCHABLE
        .iter()
        .map(|id| Ok(json!({"id": id, "enabled": enabled(&conn, id)?})))
        .collect::<anyhow::Result<Vec<_>>>()?;
    Ok(Json(json!(plugins)))
}

/// `PUT /v1/plugins/{id}` with `{"enabled": bool}`.
pub async fn set(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<String>,
    Json(body): Json<Value>,
) -> Result<Json<Value>, AppError> {
    require_admin(&headers)?;
    let Some(id) = SWITCHABLE.iter().find(|plugin| **plugin == id) else {
        return Err(AppError(StatusCode::NOT_FOUND, "unknown plugin".into()));
    };
    let Some(on) = body["enabled"].as_bool() else {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "enabled must be a boolean".into(),
        ));
    };
    db::set_setting(
        &state.db.lock().unwrap(),
        &setting(id),
        if on { "true" } else { "false" },
    )?;
    Ok(Json(json!({"id": id, "enabled": on})))
}

/// Plugin-owned tables.
pub fn initialize(conn: &Connection) -> anyhow::Result<()> {
    tasks::initialize(conn)?;
    remote::initialize(conn)?;
    presence::initialize(conn)?;
    assignment::initialize(conn)?;
    notifier::initialize(conn)?;
    reminder::initialize(conn)?;
    reclaim::initialize(conn)?;
    Ok(())
}
