//! Embedded gateway backend, ported from nitecon/agent-gateway at 986d84544aae086579c72349a1afc366cef6b589.
//! Apache-2.0. Browser routing and gateway updater lifecycle are excluded; channel plugins remain compatible.
use axum::{
    extract::{Request, State},
    http::StatusCode,
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, patch, post, put},
    Router,
};
use channel::ChannelPlugin;
use db::Db;
use std::{collections::HashMap, sync::Arc};
pub mod channel;
pub mod channels;
pub mod db;
pub mod execution_history;
pub mod memory_okf;
pub mod projects;
pub mod routes;
pub mod runtime;
pub mod secure;
pub mod security;
pub mod subtasks;
pub mod task_cancel;
pub mod task_dispatch;
pub mod task_events;
pub mod task_requester;
pub mod task_targets;
pub mod ui_auth;

#[derive(Clone)]
pub struct AppState {
    pub db: Db,
    pub plugins: Arc<HashMap<String, Arc<dyn ChannelPlugin>>>,
    #[cfg(feature = "whatsapp")]
    pub whatsapp: Option<Arc<channels::whatsapp::WhatsAppPlugin>>,
    pub default_channel: String,
    pub api_key: String,
    /// When false (`GATEWAY_UI_AUTH=off`), control-panel pages are served
    /// without a browser session. Intended for loopback-only deployments.
    pub ui_auth_enabled: bool,
    /// Retention windows (days) shown on the Settings page.
    pub retention_days: u64,
    pub bot_retention_days: u64,
    pub artifact_operations: db::ArtifactOperationsEnvelope,
    pub artifact_body_schema_enabled: bool,
    pub artifact_auth_enforced: bool,
    /// Set by the background update checker when a newer release is available.
    pub update_available: Arc<std::sync::Mutex<Option<String>>>,
}

fn resolve_session_user(state: &AppState, headers: &axum::http::HeaderMap) -> Option<db::User> {
    if !state.ui_auth_enabled {
        return None;
    }
    let token = ui_auth::session_cookie(headers)?;
    let user_id = ui_auth::token_user_id(&token)?;
    let user = {
        let conn = state.db.lock().unwrap();
        db::get_user(&conn, user_id).ok().flatten()?
    };
    ui_auth::verify_session_token(&state.api_key, &user, &token, db::now_ms()).then_some(user)
}

/// Strip any client-supplied identity headers, then stamp the resolved user.
fn stamp_user(request: &mut Request, user: Option<&db::User>) {
    let headers = request.headers_mut();
    headers.remove(ui_auth::USER_HEADER);
    headers.remove(ui_auth::USER_ROLE_HEADER);
    if let Some(user) = user {
        if let Ok(v) = axum::http::HeaderValue::from_str(&ui_auth::user_header_value(user)) {
            headers.insert(ui_auth::USER_HEADER, v);
        }
        if let Ok(v) = axum::http::HeaderValue::from_str(&user.role) {
            headers.insert(ui_auth::USER_ROLE_HEADER, v);
        }
    }
}

async fn bearer_auth(State(state): State<AppState>, mut request: Request, next: Next) -> Response {
    if let Some(peer) = request.extensions().get::<secure::Principal>().cloned() {
        stamp_user(&mut request, None);
        let value = format!("0:{}:{}", peer.node.node_id, peer.node.node_id);
        request
            .headers_mut()
            .insert(ui_auth::USER_HEADER, value.parse().unwrap());
        request.headers_mut().insert(
            ui_auth::USER_ROLE_HEADER,
            if peer.node.role == "administrator" {
                "admin"
            } else {
                "member"
            }
            .parse()
            .unwrap(),
        );
        return next.run(request).await;
    }

    let token = request
        .headers()
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "));

    if token.map(|t| t == state.api_key).unwrap_or(false) {
        stamp_user(&mut request, None);
        return next.run(request).await;
    }
    // Same-origin XHR from the control panel: session cookie + custom header.
    if ui_auth::is_ui_xhr(request.headers()) {
        if let Some(user) = resolve_session_user(&state, request.headers()) {
            stamp_user(&mut request, Some(&user));
            return next.run(request).await;
        }
        // Loopback deployments without login still get their XHR through.
        if !state.ui_auth_enabled {
            stamp_user(&mut request, None);
            return next.run(request).await;
        }
    }
    StatusCode::UNAUTHORIZED.into_response()
}

/// Existing backend API surface, without browser routes or external workers.
pub fn api_router(state: AppState) -> Router {
    Router::new()
        .route("/v1/tasks/capabilities", get(task_requester::capabilities))
        .route("/v1/projects/{ident}/tasks/{id}/requester", put(task_requester::set))
        .route("/v1/projects/{ident}/task-targets", get(task_targets::list).post(task_targets::heartbeat))
        .route("/v1/projects/{ident}/tasks/{id}/reassign", post(routes::reassign_task_handler))
        .route("/v1/projects/{ident}/tasks/{id}/assign", post(routes::assign_task_handler))
        .route("/v1/projects/{ident}/tasks/{id}/subtasks", get(subtasks::get).post(subtasks::create))
        .route("/v1/tasks/stream", get(task_events::connect))
        .route("/v1/task-events", get(task_events::events))
        .route("/v1/task-events/consumers", get(task_events::consumers))
        .route("/v1/execution/runs", get(task_events::history))
        .route(
            "/v1/projects",
            get(routes::list_projects_handler).post(routes::register_project),
        )
        .route(
            "/v1/projects/{ident}/messages",
            get(routes::list_message_threads).post(routes::send_message),
        )
        .route(
            "/v1/projects/{ident}/messages/human",
            post(routes::post_human_message),
        )
        .route(
            "/v1/projects/{ident}/messages/{id}/thread",
            get(routes::get_message_thread),
        )
        .route(
            "/v1/projects/{ident}/messages/resolve",
            post(routes::resolve_messages_bulk),
        )
        .route(
            "/v1/projects/{ident}/messages/{id}/resolve",
            post(routes::resolve_message),
        )
        .route(
            "/v1/projects/{ident}/messages/{id}/reopen",
            post(routes::reopen_message),
        )
        .route(
            "/v1/projects/{ident}/links",
            get(routes::list_project_links).post(routes::add_project_link),
        )
        .route(
            "/v1/projects/{ident}/links/{id}",
            axum::routing::delete(routes::delete_project_link),
        )
        .route(
            "/v1/projects/{ident}/room",
            post(routes::create_project_room),
        )
        .route(
            "/v1/projects/{ident}/messages/unread",
            get(routes::get_unread_messages),
        )
        .route(
            "/v1/projects/{ident}/messages/{id}/confirm",
            post(routes::confirm_message),
        )
        .route(
            "/v1/projects/{ident}/messages/{id}/reply",
            post(routes::reply_to_message),
        )
        .route(
            "/v1/projects/{ident}/messages/{id}/action",
            post(routes::taking_action_on),
        )
        .route(
            "/v1/skills",
            get(routes::list_skills_handler).post(routes::upsert_named_skill_json),
        )
        .route(
            "/v1/skills/{name}",
            put(routes::upload_skill)
                .post(routes::upsert_skill_json)
                .get(routes::download_skill)
                .delete(routes::delete_skill_handler),
        )
        .route(
            "/v1/skills/{name}/multipart",
            post(routes::upload_skill_multipart),
        )
        .route("/v1/skills/{name}/content", get(routes::get_skill_content))
        .route(
            "/v1/projects/{ident}/tasks",
            get(routes::list_tasks_handler).post(routes::create_task_handler),
        )
        .merge(task_dispatch::router())
        .route(
            "/v1/projects/{ident}/tasks/delegate",
            post(routes::delegate_task_handler),
        )
        .route(
            "/v1/projects/{ident}/tasks/reorder",
            post(routes::reorder_tasks_handler),
        )
        .route(
            "/v1/projects/{ident}/tasks/{id}",
            get(routes::get_task_handler)
                .patch(routes::update_task_handler)
                .delete(routes::delete_task_handler),
        )
        .route(
            "/v1/projects/{ident}/tasks/{id}/cancellation",
            get(task_cancel::get_handler).post(task_cancel::create_handler),
        )
        .route(
            "/v1/projects/{ident}/tasks/{id}/comments",
            post(routes::add_comment_handler),
        )
        .route(
            "/v1/projects/{ident}/tasks/{id}/delegation",
            patch(routes::update_delegation_handler),
        )
        .route(
            "/v1/projects/{ident}/repo",
            patch(routes::update_project_repo_mapping),
        )
        .route(
            "/v1/projects/{ident}/archive",
            post(routes::archive_project),
        )
        .route(
            "/v1/projects/{ident}/restore",
            post(routes::restore_project),
        )
        .route(
            "/v1/projects/{ident}/memories",
            get(routes::list_memories_handler).post(routes::push_memories_handler),
        )
        .route(
            "/v1/projects/{ident}/memories/pull",
            post(routes::pull_memories_handler),
        )
        .route(
            "/v1/projects/{ident}/memories/push",
            post(routes::push_memories_handler),
        )
        .route("/v1/memories/capabilities", get(routes::memory_capabilities_handler))
        .route(
            "/v1/memories/projects",
            get(routes::discover_memory_projects_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts",
            get(routes::list_artifacts_handler).post(routes::create_artifact_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}",
            get(routes::get_artifact_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/versions",
            get(routes::list_artifact_versions_handler)
                .post(routes::create_artifact_version_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/versions/{version_id}",
            get(routes::get_artifact_version_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/versions/{version_id}/diff",
            get(routes::diff_artifact_version_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/comments",
            get(routes::list_artifact_comments_handler)
                .post(routes::create_artifact_comment_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/comments/{comment_id}/resolve",
            post(routes::resolve_artifact_comment_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/comments/{comment_id}/reopen",
            post(routes::reopen_artifact_comment_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/contributions",
            get(routes::list_artifact_contributions_handler)
                .post(routes::create_artifact_contribution_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/links",
            get(routes::list_artifact_links_handler).post(routes::create_artifact_link_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/workflow-runs",
            get(routes::list_artifact_workflow_runs_handler)
                .post(routes::start_artifact_workflow_run_handler),
        )
        .route(
            "/v1/projects/{ident}/artifacts/{artifact_id}/workflow-runs/{workflow_run_id}",
            get(routes::get_artifact_workflow_run_handler)
                .patch(routes::complete_artifact_workflow_run_handler),
        )
        .route(
            "/v1/projects/{ident}/design-reviews",
            post(routes::create_design_review_handler),
        )
        .route(
            "/v1/projects/{ident}/design-reviews/{artifact_id}/rounds",
            post(routes::create_design_review_round_handler),
        )
        .route(
            "/v1/projects/{ident}/design-reviews/{artifact_id}/rounds/{workflow_run_id}/contributions",
            post(routes::create_design_review_contribution_handler),
        )
        .route(
            "/v1/projects/{ident}/design-reviews/{artifact_id}/rounds/{workflow_run_id}/synthesis",
            post(routes::create_design_review_synthesis_handler),
        )
        .route(
            "/v1/projects/{ident}/design-reviews/{artifact_id}/state",
            post(routes::update_design_review_state_handler),
        )
        .route(
            "/v1/projects/{ident}/design-reviews/{artifact_id}/contributions",
            get(routes::list_design_review_contributions_handler),
        )
        .route(
            "/v1/projects/{ident}/specs",
            post(routes::create_spec_handler),
        )
        .route(
            "/v1/projects/{ident}/specs/{artifact_id}/versions",
            post(routes::create_spec_version_handler),
        )
        .route(
            "/v1/projects/{ident}/specs/{artifact_id}/accept",
            post(routes::accept_spec_version_handler),
        )
        .route(
            "/v1/projects/{ident}/specs/{artifact_id}/manifest",
            get(routes::get_spec_manifest_handler),
        )
        .route(
            "/v1/projects/{ident}/specs/{artifact_id}/manifest/{manifest_item_id}",
            get(routes::get_spec_manifest_item_handler),
        )
        .route(
            "/v1/projects/{ident}/specs/{artifact_id}/generate-tasks",
            post(routes::generate_spec_tasks_handler),
        )
        .route(
            "/v1/projects/{ident}/specs/{artifact_id}/link-task",
            post(routes::link_existing_spec_task_handler),
        )
        .route(
            "/v1/patterns",
            get(routes::list_patterns_handler).post(routes::create_pattern_handler),
        )
        .route(
            "/v1/patterns/{id}",
            get(routes::get_pattern_handler)
                .patch(routes::update_pattern_handler)
                .delete(routes::delete_pattern_handler),
        )
        .route(
            "/v1/patterns/{id}/comments",
            get(routes::list_pattern_comments_handler).post(routes::add_pattern_comment_handler),
        )
        .route("/v1/users", get(routes::list_users).post(routes::create_user))
        .route("/v1/users/me", get(routes::current_user))
        .route("/v1/users/me/password", post(routes::change_own_password))
        .route(
            "/v1/users/{id}",
            patch(routes::update_user).delete(routes::delete_user),
        )
        .layer(middleware::from_fn_with_state(state.clone(), bearer_auth))
        .with_state(state)
}
