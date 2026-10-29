//! API handlers ported from nitecon/agent-gateway at
//! 986d84544aae086579c72349a1afc366cef6b589 (Apache-2.0).
//! Browser rendering/login routes and their browser-only tests are omitted.
//! Retained API handler bodies, shared models and backend tests preserve the baseline.

use axum::{
    body::Bytes,
    extract::{Path, Query, State},
    http::{header, HeaderMap, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use tokio::task::spawn_blocking;
use tracing::error;

use crate::{
    channel::OutboundMessage,
    db::{self, now_ms, Message, Project},
    projects::{normalize_project_ident, sanitize_ident},
    AppState,
};

// ── Error helper ─────────────────────────────────────────────────────────────

#[derive(Debug)]
pub struct AppError(pub StatusCode, pub String);

impl IntoResponse for AppError {
    fn into_response(self) -> Response {
        error!("handler error: {} — {}", self.0, self.1);
        (self.0, Json(serde_json::json!({"error": self.1}))).into_response()
    }
}

impl<E: Into<anyhow::Error>> From<E> for AppError {
    fn from(e: E) -> Self {
        let err = e.into();
        let status = if err
            .downcast_ref::<crate::task_events::OwnershipConflict>()
            .is_some()
            || err.downcast_ref::<TaskGoalConflict>().is_some()
            || err
                .downcast_ref::<crate::task_dispatch::Conflict>()
                .is_some()
        {
            StatusCode::CONFLICT
        } else if err.downcast_ref::<db::InvalidTaskLabels>().is_some() {
            StatusCode::BAD_REQUEST
        } else {
            StatusCode::INTERNAL_SERVER_ERROR
        };
        AppError(status, format!("{err:#}"))
    }
}

type Result<T> = std::result::Result<T, AppError>;

// ── Artifact operations envelope accessor (T016 plumbing) ───────────────────
//
// T016 owns the typed runtime surface for the T004 operations envelope. The
// envelope is parsed from `std::env` exactly once per process via a
// `OnceLock`; route handlers MUST call `operations_envelope()` instead of
// reading env vars or hardcoding T004 constants. T007 will migrate this
// accessor's caller-shape into `AppState` once the envelope acquires
// per-project overrides; until then, a process-wide cache is the right
// scope (the env values are immutable across a single binary lifetime).
//
// Failures during load short-circuit to production defaults AND log a
// warning. We deliberately do NOT panic on misconfigured env values at
// load time here — that decision belongs to a future startup gate in
// `main.rs` (out of touch surface for T016). T007 SHOULD wire
// `ArtifactOperationsEnvelope::from_env()?` into the explicit startup
// path so SRE catches misconfiguration before traffic lands.

static ARTIFACT_OPERATIONS_ENVELOPE: std::sync::OnceLock<db::ArtifactOperationsEnvelope> =
    std::sync::OnceLock::new();

/// Returns the process-wide artifact operations envelope. First call loads
/// from environment variables; subsequent calls return the cached value.
/// On parse failure the call logs a warning and returns
/// `ArtifactOperationsEnvelope::production_defaults()` so the gateway
/// continues to serve with documented T004 defaults. Startup code (future
/// T007 work) SHOULD call `db::ArtifactOperationsEnvelope::from_env()`
/// directly to surface configuration errors explicitly.
#[allow(dead_code)]
pub(crate) fn operations_envelope() -> &'static db::ArtifactOperationsEnvelope {
    ARTIFACT_OPERATIONS_ENVELOPE.get_or_init(|| {
        match db::ArtifactOperationsEnvelope::from_env() {
            Ok(envelope) => envelope,
            Err(err) => {
                tracing::warn!(
                    "artifact operations envelope failed to load from env, falling back to T004 defaults: {err}"
                );
                db::ArtifactOperationsEnvelope::production_defaults()
            }
        }
    })
}

/// Extract agent identity from X-Agent-Id header, defaulting to "_default".
fn extract_agent_id(headers: &HeaderMap) -> String {
    headers
        .get("x-agent-id")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("_default")
        .to_string()
}

/// Maximum length of an auto-derived subject when the agent does not supply one.
const AUTO_SUBJECT_MAX: usize = 80;

/// Derive a subject from the body when one is not supplied: first non-empty
/// line, trimmed, capped at `AUTO_SUBJECT_MAX` characters with an ellipsis if
/// truncated. Falls back to a generic placeholder for empty bodies.
fn derive_subject(body: &str) -> String {
    let first_line = body.lines().map(str::trim).find(|l| !l.is_empty());
    match first_line {
        None => "(no content)".to_string(),
        Some(line) => {
            let count = line.chars().count();
            if count <= AUTO_SUBJECT_MAX {
                line.to_string()
            } else {
                let mut out: String = line.chars().take(AUTO_SUBJECT_MAX - 1).collect();
                out.push('…');
                out
            }
        }
    }
}

/// Apply default values for any missing structured fields and return a
/// fully-populated `OutboundMessage`. The body argument is the resolved
/// payload (caller picks between the structured `body` field and any
/// route-specific alias such as `content` or `message`).
fn build_outbound(
    agent_id: &str,
    body: String,
    subject: Option<String>,
    hostname: Option<String>,
    event_at: Option<i64>,
) -> OutboundMessage {
    let subject = subject
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| derive_subject(&body));
    let hostname = hostname
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| agent_id.to_string());
    let event_at = event_at.unwrap_or_else(now_ms);
    OutboundMessage {
        agent_id: agent_id.to_string(),
        hostname,
        subject,
        body,
        event_at,
    }
}

// -- Generic artifacts (/v1/projects/:ident/artifacts) -----------------------

#[derive(Debug, Clone)]
struct ArtifactActorHeaders {
    actor_type: String,
    agent_system: Option<String>,
    agent_id: String,
    host: Option<String>,
}

#[derive(Debug, Clone)]
struct ArtifactMutationContext {
    actor: ArtifactActorHeaders,
    idempotency_key: String,
    workflow_run_id: Option<String>,
}

#[derive(Debug, Serialize)]
struct ArtifactActorResponse {
    actor_id: String,
    actor_type: String,
    agent_system: Option<String>,
    agent_id: Option<String>,
    host: Option<String>,
    display_name: String,
}

#[derive(Debug, Serialize)]
struct ArtifactAuthorization {
    boundary: &'static str,
    required_scopes: Vec<&'static str>,
}

#[derive(Debug, Clone)]
struct ArtifactAuthorizationDecision {
    boundary: &'static str,
    required_scopes: Vec<&'static str>,
}

#[derive(Debug, Serialize)]
struct ArtifactProvenance {
    actor: ArtifactActorResponse,
    workflow_run_id: Option<String>,
    idempotency_key: String,
    request_id: String,
    created_at: i64,
    authorization: ArtifactAuthorization,
    generated_resources: Value,
    replay: bool,
    warnings: Vec<String>,
}

#[derive(Debug, Serialize)]
struct ArtifactMutationResponse<T: Serialize> {
    data: T,
    provenance: ArtifactProvenance,
}

#[derive(Debug, Serialize)]
pub struct ArtifactReadResponse<T: Serialize> {
    data: T,
    chunking_status: ChunkingStatus,
}

#[derive(Debug, Serialize)]
pub struct ArtifactDetailResponse {
    artifact: db::ArtifactSummary,
    current_version: Option<VersionReadModel>,
    accepted_version: Option<VersionReadModel>,
}

#[derive(Debug, Serialize)]
pub struct VersionReadModel {
    #[serde(flatten)]
    version: db::ArtifactVersion,
    chunking_status: ChunkingStatus,
}

#[derive(Debug, Serialize, Clone)]
pub struct ChunkingStatus {
    status: &'static str,
    current_chunk_count: usize,
    stale_chunk_count: usize,
    superseded_chunk_count: usize,
    failed_addresses: Vec<String>,
}

#[derive(Debug, Serialize)]
pub struct ArtifactDiffResponse {
    from_version_id: Option<String>,
    to_version_id: String,
    format: &'static str,
    byte_delta: isize,
    diff: String,
    chunking_status: ChunkingStatus,
}

#[derive(Debug, Deserialize)]
pub struct ListArtifactsQuery {
    kind: Option<String>,
    subkind: Option<String>,
    lifecycle_state: Option<String>,
    label: Option<String>,
    actor_id: Option<String>,
    q: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct CreateArtifactRequest {
    kind: String,
    subkind: Option<String>,
    title: String,
    labels: Option<Vec<String>>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct CreateArtifactVersionRequest {
    version_label: Option<String>,
    parent_version_id: Option<String>,
    body_format: Option<String>,
    body: Option<String>,
    structured_payload: Option<Value>,
    source_format: Option<String>,
    version_state: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct DiffQuery {
    base_version_id: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct CreateContributionRequest {
    target_kind: String,
    target_id: String,
    contribution_kind: String,
    phase: Option<String>,
    role: String,
    read_set: Option<Value>,
    body_format: Option<String>,
    body: String,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct CreateCommentRequest {
    target_kind: String,
    target_id: String,
    child_address: Option<String>,
    parent_comment_id: Option<String>,
    body: String,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ResolveCommentRequest {
    resolution_note: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ReopenCommentRequest {
    note_body: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ListLinksQuery {
    link_type: Option<String>,
    source_kind: Option<String>,
    source_id: Option<String>,
    target_kind: Option<String>,
    target_id: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct CreateLinkRequest {
    link_type: String,
    source_kind: String,
    source_id: String,
    source_version_id: Option<String>,
    source_child_address: Option<String>,
    target_kind: String,
    target_id: String,
    target_version_id: Option<String>,
    target_child_address: Option<String>,
    supersedes_link_id: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct StartWorkflowRunRequest {
    workflow_kind: String,
    phase: Option<String>,
    round_id: Option<String>,
    participant_actor_ids: Option<Vec<String>>,
    source_artifact_version_id: Option<String>,
    read_set: Option<Value>,
    is_resumable: Option<bool>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct CompleteWorkflowRunRequest {
    state: String,
    failure_reason: Option<String>,
    generated_contribution_ids: Option<Vec<String>>,
    generated_version_ids: Option<Vec<String>>,
    generated_task_ids: Option<Vec<String>>,
    generated_link_ids: Option<Vec<String>>,
    generated_chunk_ids: Option<Vec<String>>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct SpecImportRequest {
    title: String,
    labels: Option<Vec<String>>,
    body: Option<String>,
    manifest: Value,
    file_bodies: Option<std::collections::HashMap<String, String>>,
    source_doc: Option<String>,
    source_artifact_id: Option<String>,
    source_artifact_version_id: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct SpecVersionRequest {
    version_label: Option<String>,
    parent_version_id: Option<String>,
    body: Option<String>,
    manifest: Value,
    file_bodies: Option<std::collections::HashMap<String, String>>,
    source_doc: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct SpecAcceptRequest {
    version_id: String,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct SpecManifestQuery {
    version_id: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct GenerateSpecTasksRequest {
    confirmed: bool,
    manifest_item_ids: Option<Vec<String>>,
    reporter: Option<String>,
    hostname: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct LinkSpecTaskRequest {
    version_id: Option<String>,
    manifest_item_id: String,
    task_id: String,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct DesignReviewCreateRequest {
    title: String,
    labels: Option<Vec<String>>,
    body: Option<String>,
    body_format: Option<String>,
    source_artifact_id: Option<String>,
    source_artifact_version_id: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct DesignReviewRoundRequest {
    round_id: Option<String>,
    participant_actor_ids: Option<Vec<String>>,
    source_artifact_version_id: String,
    read_set: Option<Value>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct DesignReviewContributionRequest {
    phase: String,
    role: Option<String>,
    reviewed_version_id: Option<String>,
    read_set: Option<Value>,
    body_format: Option<String>,
    body: String,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct DesignReviewSynthesisRequest {
    reviewed_version_id: Option<String>,
    read_set: Value,
    body_format: Option<String>,
    body: String,
    create_version: Option<bool>,
    version_label: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct DesignReviewStateRequest {
    lifecycle_state: Option<String>,
    review_state: Option<String>,
    note: Option<String>,
    actor_display_name: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct ListDesignReviewContributionsQuery {
    round_id: Option<String>,
    phase: Option<String>,
    role: Option<String>,
    reviewed_version_id: Option<String>,
    read_set_contains: Option<String>,
}

#[derive(Debug, Serialize, Deserialize, Clone)]
pub struct SpecManifestItem {
    manifest_item_id: String,
    phase: Option<String>,
    task_code: Option<String>,
    team: Option<String>,
    title: String,
    status: Option<String>,
    dependencies: Vec<String>,
    labels: Vec<String>,
    touch_surface: Vec<String>,
    acceptance_criteria: Vec<String>,
    validation_plan: Vec<String>,
    gateway_task_id: Option<String>,
    spec_file: Option<String>,
    spec_body: Option<String>,
    metadata: Value,
}

#[derive(Debug, Serialize)]
pub struct SpecManifestResponse {
    artifact_id: String,
    artifact_version_id: String,
    manifest: Value,
    items: Vec<SpecManifestItem>,
    stability_policy: Value,
}

#[derive(Debug, Serialize)]
pub struct GenerateSpecTasksResponse {
    artifact_id: String,
    artifact_version_id: String,
    workflow_run_id: String,
    generated_task_ids: Vec<String>,
    generated_link_ids: Vec<String>,
    items: Vec<Value>,
    replayed: bool,
}

fn parse_scope_header(headers: &HeaderMap) -> std::collections::BTreeSet<String> {
    headers
        .get("x-agent-scopes")
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
        .split(|c: char| c == ',' || c.is_whitespace())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(str::to_string)
        .collect()
}

fn scope_grants(held: &std::collections::BTreeSet<String>, required: &str) -> bool {
    held.contains("*")
        || held.contains(required)
        || required
            .split_once('.')
            .is_some_and(|(prefix, _)| held.contains(&format!("{prefix}.*")))
}

fn require_artifact_authorization(
    state: &AppState,
    headers: &HeaderMap,
    project_ident: &str,
    required_scopes: Vec<&'static str>,
) -> Result<ArtifactAuthorizationDecision> {
    if !state.artifact_auth_enforced {
        return Ok(ArtifactAuthorizationDecision {
            boundary: "trusted-single-tenant",
            required_scopes,
        });
    }

    let expected_project = normalize_project_ident(project_ident);
    let authorized_project = headers
        .get("x-agent-project")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(normalize_project_ident);
    if authorized_project.as_deref() != Some(expected_project.as_str()) {
        return Err(AppError(
            StatusCode::FORBIDDEN,
            "artifact_authorization_forbidden: missing or mismatched x-agent-project".to_string(),
        ));
    }

    let held_scopes = parse_scope_header(headers);
    let missing_scopes: Vec<&'static str> = required_scopes
        .iter()
        .copied()
        .filter(|scope| !scope_grants(&held_scopes, scope))
        .collect();
    if !missing_scopes.is_empty() {
        return Err(AppError(
            StatusCode::FORBIDDEN,
            format!(
                "artifact_authorization_forbidden: missing scopes {}",
                missing_scopes.join(",")
            ),
        ));
    }

    Ok(ArtifactAuthorizationDecision {
        boundary: "project-scoped",
        required_scopes,
    })
}

fn require_artifact_read(
    state: &AppState,
    headers: &HeaderMap,
    project_ident: &str,
) -> Result<ArtifactAuthorizationDecision> {
    require_artifact_authorization(state, headers, project_ident, vec!["artifact.read"])
}

fn require_quota_override_authorization(
    state: &AppState,
    headers: &HeaderMap,
    project_ident: &str,
) -> Result<()> {
    let requested = headers
        .get("x-artifact-quota-override")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .is_some_and(|v| matches!(v.to_ascii_lowercase().as_str(), "1" | "true" | "yes"));
    if requested {
        require_artifact_authorization(state, headers, project_ident, vec!["project.administer"])?;
    }
    Ok(())
}

fn artifact_body_schema_allowed(
    state: &AppState,
    body_format: &str,
    payload: Option<&Value>,
) -> Result<()> {
    if state.artifact_body_schema_enabled || (body_format == "markdown" && payload.is_none()) {
        Ok(())
    } else {
        Err(AppError(
            StatusCode::SERVICE_UNAVAILABLE,
            "artifact_body_schema_disabled".to_string(),
        ))
    }
}

fn parse_artifact_actor_headers(
    headers: &HeaderMap,
    mutation: bool,
) -> Result<ArtifactActorHeaders> {
    let agent_id = headers
        .get("x-agent-id")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(str::to_string);
    if mutation && agent_id.as_deref().unwrap_or("_default") == "_default" {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "x_agent_id_required".to_string(),
        ));
    }

    let actor_type = headers
        .get("x-actor-type")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or("agent")
        .to_ascii_lowercase();
    if !matches!(actor_type.as_str(), "user" | "agent" | "system") {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "invalid_x_actor_type".to_string(),
        ));
    }

    let agent_system = headers
        .get("x-agent-system")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .map(|v| v.to_ascii_lowercase());
    if actor_type == "agent" {
        match agent_system.as_deref() {
            Some("claude" | "codex" | "gemini" | "other") => {}
            Some(_) => {
                return Err(AppError(
                    StatusCode::BAD_REQUEST,
                    "invalid_x_agent_system".to_string(),
                ));
            }
            None if mutation => {
                return Err(AppError(
                    StatusCode::BAD_REQUEST,
                    "x_agent_system_required".to_string(),
                ));
            }
            None => {}
        }
    }

    Ok(ArtifactActorHeaders {
        actor_type,
        agent_system,
        agent_id: agent_id.unwrap_or_else(|| "_default".to_string()),
        host: headers
            .get("x-host")
            .and_then(|v| v.to_str().ok())
            .map(str::trim)
            .filter(|v| !v.is_empty())
            .map(str::to_string),
    })
}

fn parse_mutation_context(headers: &HeaderMap) -> Result<ArtifactMutationContext> {
    let key = headers
        .get("idempotency-key")
        .and_then(|v| v.to_str().ok())
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .ok_or_else(|| {
            AppError(
                StatusCode::BAD_REQUEST,
                "idempotency_key_required".to_string(),
            )
        })?
        .to_string();
    if key.len() > 255 {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "idempotency_key_too_long".to_string(),
        ));
    }

    Ok(ArtifactMutationContext {
        actor: parse_artifact_actor_headers(headers, true)?,
        idempotency_key: key,
        workflow_run_id: headers
            .get("x-workflow-run-id")
            .and_then(|v| v.to_str().ok())
            .map(str::trim)
            .filter(|v| !v.is_empty())
            .map(str::to_string),
    })
}

fn trim_required(value: &str, field: &'static str) -> Result<String> {
    let trimmed = value.trim();
    if trimmed.is_empty() {
        Err(AppError(
            StatusCode::BAD_REQUEST,
            format!("{field}_required"),
        ))
    } else {
        Ok(trimmed.to_string())
    }
}

fn validate_artifact_kind(kind: &str) -> Result<()> {
    if matches!(kind, "design_review" | "spec") {
        Ok(())
    } else {
        Err(AppError(
            StatusCode::BAD_REQUEST,
            "invalid_artifact_kind".to_string(),
        ))
    }
}

fn validate_body_format(format: &str) -> Result<()> {
    if matches!(format, "markdown") {
        Ok(())
    } else {
        Err(AppError(
            StatusCode::BAD_REQUEST,
            "invalid_body_format".to_string(),
        ))
    }
}

fn validate_read_set_required(
    phase: Option<&str>,
    kind: &str,
    read_set: Option<&Value>,
) -> Result<()> {
    if read_set.is_none() && (matches!(phase, Some("pass_2" | "synthesis")) || kind == "synthesis")
    {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "read_set_required".to_string(),
        ));
    }
    Ok(())
}

fn resolve_artifact_actor(
    conn: &rusqlite::Connection,
    headers: &ArtifactActorHeaders,
    display_name: Option<&str>,
) -> anyhow::Result<db::ArtifactActor> {
    let display = display_name
        .map(str::trim)
        .filter(|v| !v.is_empty())
        .unwrap_or(&headers.agent_id);
    db::resolve_artifact_actor(
        conn,
        &db::ArtifactActorIdentity {
            actor_type: &headers.actor_type,
            agent_system: headers.agent_system.as_deref(),
            agent_system_label: None,
            agent_id: Some(&headers.agent_id),
            host: headers.host.as_deref(),
            display_name: display,
            runtime_metadata: None,
        },
    )
}

fn map_db_error(err: anyhow::Error) -> AppError {
    if let Some(ops) = err.downcast_ref::<db::OperationsError>() {
        return match ops {
            db::OperationsError::SizeLimit { kind, .. } => {
                let token = kind.token();
                let code = match *kind {
                    db::SizeLimitKind::ArtifactLabelsCount
                    | db::SizeLimitKind::ArtifactLabelBytes
                    | db::SizeLimitKind::ReadSetRefs => token.to_string(),
                    _ => format!("{token}_too_large"),
                };
                let status = match *kind {
                    db::SizeLimitKind::ArtifactLabelsCount
                    | db::SizeLimitKind::ArtifactLabelBytes
                    | db::SizeLimitKind::ReadSetRefs => StatusCode::BAD_REQUEST,
                    _ => StatusCode::PAYLOAD_TOO_LARGE,
                };
                AppError(status, code)
            }
            db::OperationsError::QuotaHardReject { counter, .. } => AppError(
                StatusCode::TOO_MANY_REQUESTS,
                format!("quota_{}_exceeded", counter.token()),
            ),
            db::OperationsError::InvalidEnvValue { .. }
            | db::OperationsError::InvalidQuotaThresholds { .. } => {
                AppError(StatusCode::INTERNAL_SERVER_ERROR, format!("{err:#}"))
            }
        };
    }

    let text = format!("{err:#}");
    let lower = text.to_ascii_lowercase();
    if lower.contains("not found") {
        AppError(StatusCode::NOT_FOUND, text)
    } else if lower.contains("required")
        || lower.contains("invalid")
        || lower.contains("missing")
        || lower.contains("does not belong")
        || lower.contains("cannot")
        || lower.contains("unsupported")
        || lower.contains("not retryable")
        || lower.contains("check constraint failed")
    {
        AppError(StatusCode::BAD_REQUEST, text)
    } else {
        AppError(StatusCode::INTERNAL_SERVER_ERROR, text)
    }
}

fn generated_resource(key: &str, value: impl Into<Value>) -> Value {
    serde_json::json!({ key: value.into() })
}

fn generated_workflow_resources(run: &db::WorkflowRun) -> Value {
    serde_json::json!({
        "workflow_run_id": run.workflow_run_id,
        "state": run.state,
        "generated_contribution_ids": run.generated_contribution_ids,
        "generated_version_ids": run.generated_version_ids,
        "generated_task_ids": run.generated_task_ids,
        "generated_link_ids": run.generated_link_ids,
        "generated_chunk_ids": run.generated_chunk_ids,
    })
}

fn build_artifact_provenance(
    actor: db::ArtifactActor,
    context: &ArtifactMutationContext,
    authorization: ArtifactAuthorizationDecision,
    generated_resources: Value,
    warnings: Vec<db::QuotaWarning>,
    replay: bool,
) -> ArtifactProvenance {
    ArtifactProvenance {
        actor: ArtifactActorResponse {
            actor_id: actor.actor_id,
            actor_type: actor.actor_type,
            agent_system: actor.agent_system,
            agent_id: actor.agent_id,
            host: actor.host,
            display_name: actor.display_name,
        },
        workflow_run_id: context.workflow_run_id.clone(),
        idempotency_key: context.idempotency_key.clone(),
        request_id: uuid::Uuid::now_v7().to_string(),
        created_at: db::now_ms(),
        authorization: ArtifactAuthorization {
            boundary: authorization.boundary,
            required_scopes: authorization.required_scopes,
        },
        generated_resources,
        replay,
        warnings: warnings.into_iter().map(|w| w.token()).collect(),
    }
}

fn mutation_response<T: Serialize>(
    status: StatusCode,
    data: T,
    provenance: ArtifactProvenance,
) -> Response {
    (status, Json(ArtifactMutationResponse { data, provenance })).into_response()
}

fn status_for_replay(replayed: bool) -> StatusCode {
    if replayed {
        StatusCode::OK
    } else {
        StatusCode::CREATED
    }
}

/// Canonical metric `result` label for create-style artifact mutations.
/// Replayed inserts (idempotency-key hits) report `"replayed"`; first-time
/// inserts report `"created"`. Centralized so every handler reports the same
/// label values to the operations pipeline.
fn result_label(replayed: bool) -> &'static str {
    if replayed {
        "replayed"
    } else {
        "created"
    }
}

fn emit_artifact_metric(name: &str, labels: &[(&str, &str)]) {
    tracing::info!(target: "gateway_metrics", metric = name, labels = ?labels, "artifact metric");
}

/// Common entry validation for every artifact mutation route handler:
/// parse the mandatory mutation envelope (actor headers + idempotency-key,
/// plus optional workflow-run id).
/// Returns the parsed [`ArtifactMutationContext`] on success.
///
/// Endpoint-specific request validation (`trim_required`, body-format checks,
/// read-set requirements, etc.) intentionally stays in the handler so this
/// helper remains a stable mutation envelope rather than a generic abstraction.
fn begin_artifact_mutation(
    state: &AppState,
    headers: &HeaderMap,
    project_ident: &str,
    scopes: Vec<&'static str>,
) -> Result<(ArtifactMutationContext, ArtifactAuthorizationDecision)> {
    let authorization = require_artifact_authorization(state, headers, project_ident, scopes)?;
    require_quota_override_authorization(state, headers, project_ident)?;
    Ok((parse_mutation_context(headers)?, authorization))
}

/// Build the standard mutation response envelope: status (replay-aware),
/// provenance (actor + idempotency-key + generated_resources + warnings +
/// authorization scopes), and JSON payload. Every artifact mutation handler
/// funnels its happy-path response through this helper so provenance fields,
/// replay status semantics, and the response shape stay consistent.
fn finalize_mutation<T: Serialize>(
    actor: db::ArtifactActor,
    context: &ArtifactMutationContext,
    authorization: ArtifactAuthorizationDecision,
    generated_resources: Value,
    warnings: Vec<db::QuotaWarning>,
    replayed: bool,
    data: T,
) -> Response {
    let provenance = build_artifact_provenance(
        actor,
        context,
        authorization,
        generated_resources,
        warnings,
        replayed,
    );
    mutation_response(status_for_replay(replayed), data, provenance)
}

/// Build the response envelope for mutations whose status is always `200 OK`
/// and which do not surface idempotent replay (e.g. comment resolve/reopen,
/// workflow-run complete). Keeps the provenance construction signature uniform
/// with [`finalize_mutation`] for handlers that branch on replay state.
fn finalize_ok_mutation<T: Serialize>(
    actor: db::ArtifactActor,
    context: &ArtifactMutationContext,
    authorization: ArtifactAuthorizationDecision,
    generated_resources: Value,
    warnings: Vec<db::QuotaWarning>,
    data: T,
) -> Response {
    let provenance = build_artifact_provenance(
        actor,
        context,
        authorization,
        generated_resources,
        warnings,
        false,
    );
    mutation_response(StatusCode::OK, data, provenance)
}

fn chunking_status_for(
    artifact: &db::ArtifactSummary,
    chunks: &[db::ArtifactChunk],
) -> ChunkingStatus {
    let freshness_version = artifact
        .accepted_version_id
        .as_deref()
        .or(artifact.current_version_id.as_deref());
    let mut current = 0usize;
    let mut stale = 0usize;
    let mut superseded = 0usize;
    let mut failed_addresses = Vec::new();
    for chunk in chunks {
        if chunk.superseded_by_chunk_id.is_some() {
            superseded += 1;
        } else if Some(chunk.artifact_version_id.as_str()) == freshness_version {
            current += 1;
        } else {
            stale += 1;
        }
        if let Some(Value::Object(metadata)) = &chunk.metadata {
            let failed = metadata
                .get("status")
                .or_else(|| metadata.get("chunking_status"))
                .and_then(Value::as_str)
                == Some("failed");
            if failed {
                failed_addresses.push(chunk.child_address.clone());
            }
        }
    }
    let status = if !failed_addresses.is_empty() {
        "partial"
    } else if stale > 0 {
        "stale"
    } else if current > 0 {
        "current"
    } else {
        "none"
    };
    ChunkingStatus {
        status,
        current_chunk_count: current,
        stale_chunk_count: stale,
        superseded_chunk_count: superseded,
        failed_addresses,
    }
}

fn version_status(
    artifact: &db::ArtifactSummary,
    version: db::ArtifactVersion,
    chunks: &[db::ArtifactChunk],
) -> VersionReadModel {
    let selected: Vec<_> = chunks
        .iter()
        .filter(|chunk| chunk.artifact_version_id == version.artifact_version_id)
        .cloned()
        .collect();
    VersionReadModel {
        version,
        chunking_status: chunking_status_for(artifact, &selected),
    }
}

/// Shared retryability gate for any artifact mutation that references an existing
/// `workflow_run`. Cancelled runs and non-resumable failed runs cannot be reused;
/// every other state (running, succeeded, resumable-failed) is allowed to proceed
/// — replay/idempotency-key handling at the DB layer still owns terminal-state
/// dedup semantics. Centralizing this here keeps artifact-scoped and project-
/// scoped (link) handlers from drifting on these error messages, which
/// `map_db_error` matches against to produce `BAD_REQUEST`.
fn ensure_workflow_run_retryable(run: &db::WorkflowRun) -> anyhow::Result<()> {
    match run.state.as_str() {
        "cancelled" => anyhow::bail!("cancelled workflow_run is not retryable"),
        "failed" if !run.is_resumable => {
            anyhow::bail!("non-resumable failed workflow_run is not retryable")
        }
        _ => Ok(()),
    }
}

/// Load and validate a workflow run that must belong to a specific artifact.
/// Returns `Ok(None)` when no run id is supplied, `Ok(Some(run))` for a
/// retryable run, and `Err` for not-found, cross-artifact, or non-retryable
/// state.
fn validate_workflow_run_for_mutation(
    conn: &rusqlite::Connection,
    artifact_id: &str,
    workflow_run_id: Option<&str>,
) -> anyhow::Result<Option<db::WorkflowRun>> {
    let Some(run_id) = workflow_run_id else {
        return Ok(None);
    };
    let run = db::get_workflow_run(conn, run_id)?
        .ok_or_else(|| anyhow::anyhow!("workflow_run not found"))?;
    if run.artifact_id != artifact_id {
        anyhow::bail!("workflow_run does not belong to artifact");
    }
    ensure_workflow_run_retryable(&run)?;
    Ok(Some(run))
}

/// Project-scoped workflow run lookup for link creation. Unlike
/// [`validate_workflow_run_for_mutation`], this does not bind the run to a
/// specific artifact — links are project-scoped and may reference resources
/// across artifacts — but it still applies the canonical retryability gate so
/// link creation cannot resurrect cancelled or non-resumable-failed runs.
fn load_workflow_run_for_link(
    conn: &rusqlite::Connection,
    workflow_run_id: Option<&str>,
) -> anyhow::Result<Option<db::WorkflowRun>> {
    let Some(run_id) = workflow_run_id else {
        return Ok(None);
    };
    let run = db::get_workflow_run(conn, run_id)?
        .ok_or_else(|| anyhow::anyhow!("workflow_run not found"))?;
    ensure_workflow_run_retryable(&run)?;
    Ok(Some(run))
}

fn complete_resumed_run(
    conn: &rusqlite::Connection,
    run: Option<&db::WorkflowRun>,
    contribution_id: Option<String>,
    version_id: Option<String>,
    link_id: Option<String>,
) -> anyhow::Result<()> {
    let Some(run) = run else {
        return Ok(());
    };
    if !(run.state == "failed" && run.is_resumable) {
        return Ok(());
    }

    let mut contribution_ids = run.generated_contribution_ids.clone();
    let mut version_ids = run.generated_version_ids.clone();
    let mut link_ids = run.generated_link_ids.clone();
    if let Some(id) = contribution_id {
        if !contribution_ids.contains(&id) {
            contribution_ids.push(id);
        }
    }
    if let Some(id) = version_id {
        if !version_ids.contains(&id) {
            version_ids.push(id);
        }
    }
    if let Some(id) = link_id {
        if !link_ids.contains(&id) {
            link_ids.push(id);
        }
    }
    db::update_workflow_run(
        conn,
        &run.workflow_run_id,
        &db::WorkflowRunUpdate {
            state: Some("succeeded"),
            failure_reason: Some(None),
            generated_contribution_ids: Some(&contribution_ids),
            generated_version_ids: Some(&version_ids),
            generated_task_ids: Some(&run.generated_task_ids),
            generated_link_ids: Some(&link_ids),
            generated_chunk_ids: Some(&run.generated_chunk_ids),
            ended_at: None,
        },
    )?;
    Ok(())
}

fn spec_item_stability_policy() -> Value {
    serde_json::json!({
        "stable_id_field": "manifest_item_id",
        "fallback_heuristic": "phase_id plus task id/code from source-adjacent manifests",
        "unchanged": "preserve manifest_item_id",
        "renamed": "preserve manifest_item_id when the task id/code is unchanged",
        "split": "keep the original manifest_item_id for the continuing item and issue new ids for split-out work",
        "merged": "issue a new manifest_item_id unless the manifest explicitly declares the surviving id",
        "deleted": "do not delete existing gateway task or artifact links; future generations simply omit the item",
        "collision": "explicit duplicate manifest_item_id values are rejected"
    })
}

fn string_array_field(value: &Value, keys: &[&str]) -> Vec<String> {
    for key in keys {
        if let Some(values) = value.get(*key).and_then(Value::as_array) {
            return values
                .iter()
                .filter_map(Value::as_str)
                .map(str::trim)
                .filter(|v| !v.is_empty())
                .map(str::to_string)
                .collect();
        }
        if let Some(value) = value.get(*key).and_then(Value::as_str) {
            let trimmed = value.trim();
            if !trimmed.is_empty() {
                return vec![trimmed.to_string()];
            }
        }
    }
    Vec::new()
}

fn optional_string_field_value(value: &Value, keys: &[&str]) -> Option<String> {
    keys.iter().find_map(|key| {
        value
            .get(*key)
            .and_then(Value::as_str)
            .map(str::trim)
            .filter(|v| !v.is_empty())
            .map(str::to_string)
    })
}

fn manifest_item_id(phase_id: Option<&str>, item: &Value) -> Option<String> {
    optional_string_field_value(item, &["manifest_item_id", "stable_id"]).or_else(|| {
        optional_string_field_value(item, &["id", "task_code", "code"])
            .map(|id| phase_id.map(|phase| format!("{phase}:{id}")).unwrap_or(id))
    })
}

fn normalize_spec_manifest(
    manifest: &Value,
    file_bodies: Option<&std::collections::HashMap<String, String>>,
) -> Result<Vec<SpecManifestItem>> {
    let mut items = Vec::new();
    let mut seen = std::collections::BTreeSet::new();

    if let Some(raw_items) = manifest.get("items").and_then(Value::as_array) {
        for item in raw_items {
            let manifest_item_id = manifest_item_id(None, item).ok_or_else(|| {
                AppError(
                    StatusCode::BAD_REQUEST,
                    "manifest_item_id_required".to_string(),
                )
            })?;
            if !seen.insert(manifest_item_id.clone()) {
                return Err(AppError(
                    StatusCode::BAD_REQUEST,
                    "duplicate_manifest_item_id".to_string(),
                ));
            }
            let title = optional_string_field_value(item, &["title"])
                .ok_or_else(|| AppError(StatusCode::BAD_REQUEST, "task_title_required".into()))?;
            let spec_file = optional_string_field_value(item, &["spec_file", "file_path"]);
            let spec_body =
                optional_string_field_value(item, &["spec_body", "body"]).or_else(|| {
                    spec_file
                        .as_deref()
                        .and_then(|path| file_bodies.and_then(|bodies| bodies.get(path)).cloned())
                });
            items.push(SpecManifestItem {
                manifest_item_id,
                phase: optional_string_field_value(item, &["phase", "phase_id"]),
                task_code: optional_string_field_value(item, &["task_code", "id", "code"]),
                team: optional_string_field_value(item, &["team"]),
                title,
                status: optional_string_field_value(item, &["status"]),
                dependencies: string_array_field(item, &["dependencies", "depends_on"]),
                labels: string_array_field(item, &["labels"]),
                touch_surface: string_array_field(item, &["touch_surface"]),
                acceptance_criteria: string_array_field(item, &["acceptance_criteria"]),
                validation_plan: string_array_field(item, &["validation_plan"]),
                gateway_task_id: optional_string_field_value(item, &["gateway_task_id"]),
                spec_file,
                spec_body,
                metadata: item.clone(),
            });
        }
    } else if let Some(phases) = manifest.get("phases").and_then(Value::as_array) {
        for phase in phases {
            let phase_id = optional_string_field_value(phase, &["id"]);
            let phase_name = optional_string_field_value(phase, &["name"]).or(phase_id.clone());
            let Some(tasks) = phase.get("tasks").and_then(Value::as_array) else {
                continue;
            };
            for task in tasks {
                let manifest_item_id =
                    manifest_item_id(phase_id.as_deref(), task).ok_or_else(|| {
                        AppError(
                            StatusCode::BAD_REQUEST,
                            "manifest_item_id_required".to_string(),
                        )
                    })?;
                if !seen.insert(manifest_item_id.clone()) {
                    return Err(AppError(
                        StatusCode::BAD_REQUEST,
                        "duplicate_manifest_item_id".to_string(),
                    ));
                }
                let title = optional_string_field_value(task, &["title"]).ok_or_else(|| {
                    AppError(StatusCode::BAD_REQUEST, "task_title_required".into())
                })?;
                let spec_file = optional_string_field_value(task, &["spec_file", "file_path"]);
                let spec_body =
                    optional_string_field_value(task, &["spec_body", "body"]).or_else(|| {
                        spec_file.as_deref().and_then(|path| {
                            file_bodies.and_then(|bodies| bodies.get(path)).cloned()
                        })
                    });
                items.push(SpecManifestItem {
                    manifest_item_id,
                    phase: phase_name.clone(),
                    task_code: optional_string_field_value(task, &["task_code", "id", "code"]),
                    team: optional_string_field_value(task, &["team"]),
                    title,
                    status: optional_string_field_value(task, &["status"]),
                    dependencies: string_array_field(task, &["dependencies", "depends_on"]),
                    labels: string_array_field(task, &["labels"]),
                    touch_surface: string_array_field(task, &["touch_surface"]),
                    acceptance_criteria: string_array_field(task, &["acceptance_criteria"]),
                    validation_plan: string_array_field(task, &["validation_plan"]),
                    gateway_task_id: optional_string_field_value(task, &["gateway_task_id"]),
                    spec_file,
                    spec_body,
                    metadata: task.clone(),
                });
            }
        }
    }

    if items.is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "manifest_items_required".to_string(),
        ));
    }
    Ok(items)
}

fn spec_structured_payload(
    manifest: Value,
    file_bodies: Option<std::collections::HashMap<String, String>>,
    source_doc: Option<String>,
    source_artifact_id: Option<String>,
    source_artifact_version_id: Option<String>,
) -> Result<Value> {
    let items = normalize_spec_manifest(&manifest, file_bodies.as_ref())?;
    Ok(serde_json::json!({
        "schema": "gateway.spec_manifest.v1",
        "source_doc": source_doc,
        "source_artifact_id": source_artifact_id,
        "source_artifact_version_id": source_artifact_version_id,
        "manifest": manifest,
        "items": items,
        "stability_policy": spec_item_stability_policy()
    }))
}

fn spec_manifest_from_version(
    artifact_id: &str,
    version: &db::ArtifactVersion,
) -> Result<SpecManifestResponse> {
    let payload = version
        .structured_payload
        .as_ref()
        .ok_or_else(|| AppError(StatusCode::BAD_REQUEST, "spec_manifest_missing".to_string()))?;
    let manifest = payload
        .get("manifest")
        .cloned()
        .unwrap_or_else(|| payload.clone());
    let items_value = payload.get("items").cloned().unwrap_or(Value::Null);
    let items: Vec<SpecManifestItem> = serde_json::from_value(items_value).map_err(|err| {
        AppError(
            StatusCode::INTERNAL_SERVER_ERROR,
            format!("invalid stored spec manifest items: {err}"),
        )
    })?;
    Ok(SpecManifestResponse {
        artifact_id: artifact_id.to_string(),
        artifact_version_id: version.artifact_version_id.clone(),
        manifest,
        items,
        stability_policy: payload
            .get("stability_policy")
            .cloned()
            .unwrap_or_else(spec_item_stability_policy),
    })
}

fn spec_task_source_block(artifact_id: &str, version_id: &str, item: &SpecManifestItem) -> String {
    format!(
        "Source:\nsource_spec_artifact_id: {artifact_id}\nsource_spec_version_id: {version_id}\nmanifest_item_id: {}\nspec_file: {}\n",
        item.manifest_item_id,
        item.spec_file.as_deref().unwrap_or("")
    )
}

fn generated_task_specification(
    artifact_id: &str,
    version_id: &str,
    item: &SpecManifestItem,
) -> String {
    let mut out = spec_task_source_block(artifact_id, version_id, item);
    out.push('\n');
    if let Some(team) = &item.team {
        out.push_str(&format!("Team: {team}\n"));
    }
    if !item.dependencies.is_empty() {
        out.push_str(&format!("Depends on: {}\n", item.dependencies.join(", ")));
    }
    if !item.touch_surface.is_empty() {
        out.push_str("\nTouch surface:\n");
        for path in &item.touch_surface {
            out.push_str(&format!("- {path}\n"));
        }
    }
    if !item.acceptance_criteria.is_empty() {
        out.push_str("\nAcceptance criteria:\n");
        for criterion in &item.acceptance_criteria {
            out.push_str(&format!("- {criterion}\n"));
        }
    }
    if !item.validation_plan.is_empty() {
        out.push_str("\nValidation plan:\n");
        for step in &item.validation_plan {
            out.push_str(&format!("- {step}\n"));
        }
    }
    if let Some(body) = &item.spec_body {
        out.push_str("\nFocused spec:\n");
        out.push_str(body);
        if !body.ends_with('\n') {
            out.push('\n');
        }
    }
    out
}

fn generated_spec_task_title(item: &SpecManifestItem) -> String {
    item.task_code
        .as_deref()
        .map(|code| format!("{code} {}", item.title))
        .unwrap_or_else(|| item.title.clone())
}

fn design_review_payload(
    source_artifact_id: Option<String>,
    source_artifact_version_id: Option<String>,
) -> Value {
    serde_json::json!({
        "workflow": "design_review",
        "source": {
            "artifact_id": source_artifact_id,
            "artifact_version_id": source_artifact_version_id,
        }
    })
}

fn read_set_has_ids(read_set: &Value, keys: &[&str]) -> bool {
    let Value::Object(map) = read_set else {
        return false;
    };
    keys.iter().any(|key| {
        map.get(*key)
            .and_then(Value::as_array)
            .is_some_and(|values| values.iter().any(|value| value.as_str().is_some()))
    })
}

fn validate_design_review_phase(phase: &str, read_set: Option<&Value>) -> Result<()> {
    if !matches!(phase, "pass_1" | "pass_2") {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "invalid_review_phase".to_string(),
        ));
    }
    validate_read_set_required(Some(phase), "review", read_set)?;
    if phase == "pass_2"
        && !read_set
            .is_some_and(|value| read_set_has_ids(value, &["contribution", "contributions"]))
    {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "pass_2_read_set_contributions_required".to_string(),
        ));
    }
    Ok(())
}

fn validate_synthesis_read_set(read_set: &Value) -> Result<()> {
    if !read_set_has_ids(read_set, &["contribution", "contributions"]) {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "synthesis_read_set_contributions_required".to_string(),
        ));
    }
    Ok(())
}

fn read_set_for_reviewed_version(reviewed_version_id: &str) -> Value {
    serde_json::json!({ "versions": [reviewed_version_id] })
}

fn item_generation_key(version_id: &str, manifest_item_id: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(version_id.as_bytes());
    hasher.update(b":");
    hasher.update(manifest_item_id.as_bytes());
    format!("spec-task-{}", hex::encode(hasher.finalize()))
}

fn body_text(version: &db::ArtifactVersion) -> String {
    if let Some(body) = &version.body {
        body.clone()
    } else if let Some(payload) = &version.structured_payload {
        serde_json::to_string_pretty(payload).unwrap_or_else(|_| payload.to_string())
    } else {
        String::new()
    }
}

fn simple_diff(from: &str, to: &str) -> String {
    let from_lines: Vec<_> = from.lines().collect();
    let to_lines: Vec<_> = to.lines().collect();
    let mut out = String::new();
    out.push_str("--- base\n+++ target\n");
    for line in &from_lines {
        if !to_lines.contains(line) {
            out.push('-');
            out.push_str(line);
            out.push('\n');
        }
    }
    for line in &to_lines {
        if !from_lines.contains(line) {
            out.push('+');
            out.push_str(line);
            out.push('\n');
        }
    }
    out
}

pub async fn list_artifacts_handler(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    Query(query): Query<ListArtifactsQuery>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<Vec<db::ArtifactSummary>>>> {
    require_artifact_read(&state, &headers, &ident)?;
    emit_artifact_metric(
        "gateway_artifact_search_requests_total",
        &[
            ("project", ident.as_str()),
            ("by", if query.q.is_some() { "query" } else { "id" }),
        ],
    );
    let db = state.db.clone();
    let artifacts = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_artifacts(
            &conn,
            &ident,
            &db::ArtifactFilters {
                kind: query.kind.as_deref(),
                subkind: query.subkind.as_deref(),
                lifecycle_state: query.lifecycle_state.as_deref(),
                label: query.label.as_deref(),
                actor_id: query.actor_id.as_deref(),
                query: query.q.as_deref(),
            },
        )
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: artifacts,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn create_artifact_handler(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    headers: HeaderMap,
    Json(body): Json<CreateArtifactRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["artifact.write"])?;
    let kind = trim_required(&body.kind, "kind")?;
    validate_artifact_kind(&kind)?;
    let title = trim_required(&body.title, "title")?;
    let labels = body.labels.unwrap_or_default();
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_project(&conn, &ident)?.ok_or_else(|| anyhow::anyhow!("project not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let result = db::create_artifact(
            &conn,
            &envelope,
            &db::ArtifactInsert {
                project_ident: &ident,
                kind: &kind,
                subkind: body.subkind.as_deref(),
                title: &title,
                labels: &labels,
                created_by_actor_id: &actor.actor_id,
            },
        )?;
        Ok::<_, anyhow::Error>((actor, result))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    emit_artifact_metric(
        "gateway_artifact_writes_total",
        &[
            ("project", write.record.project_ident.as_str()),
            ("kind", write.record.kind.as_str()),
            ("result", "created"),
        ],
    );
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated_resource("artifact_id", write.record.artifact_id.clone()),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn get_artifact_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<ArtifactDetailResponse>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let data = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let detail = db::get_artifact(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let chunks = db::list_artifact_chunks(
            &conn,
            &ident,
            &artifact_id,
            &db::ArtifactChunkFilters {
                include_superseded: true,
                ..Default::default()
            },
        )?;
        let status = chunking_status_for(&detail.artifact, &chunks);
        let current_version = detail
            .current_version
            .map(|version| version_status(&detail.artifact, version, &chunks));
        let accepted_version = detail
            .accepted_version
            .map(|version| version_status(&detail.artifact, version, &chunks));
        Ok::<_, anyhow::Error>(ArtifactReadResponse {
            data: ArtifactDetailResponse {
                artifact: detail.artifact,
                current_version,
                accepted_version,
            },
            chunking_status: status,
        })
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(data))
}

pub async fn list_artifact_versions_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<Vec<VersionReadModel>>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let data = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let versions = db::list_artifact_versions(&conn, &ident, &artifact_id)?;
        let chunks = db::list_artifact_chunks(
            &conn,
            &ident,
            &artifact_id,
            &db::ArtifactChunkFilters {
                include_superseded: true,
                ..Default::default()
            },
        )?;
        let status = chunking_status_for(&artifact, &chunks);
        Ok::<_, anyhow::Error>(ArtifactReadResponse {
            data: versions
                .into_iter()
                .map(|v| version_status(&artifact, v, &chunks))
                .collect(),
            chunking_status: status,
        })
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(data))
}

pub async fn create_artifact_version_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<CreateArtifactVersionRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["artifact_version.create"])?;
    let body_format = body
        .body_format
        .as_deref()
        .unwrap_or("markdown")
        .to_string();
    validate_body_format(&body_format)?;
    artifact_body_schema_allowed(&state, &body_format, body.structured_payload.as_ref())?;
    let version_state = body.version_state.as_deref().unwrap_or("draft").to_string();
    if !matches!(
        version_state.as_str(),
        "draft" | "under_review" | "accepted" | "superseded" | "rejected"
    ) {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "invalid_version_state".to_string(),
        ));
    }
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let ident_for_metric = ident.clone();
    let body_format_for_db = body_format.clone();
    let body_format_for_metric = body_format.clone();
    let context_for_db = context.clone();
    let workflow_run_id_for_response = context.workflow_run_id.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let run = validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let result = db::create_artifact_version(
            &conn,
            &envelope,
            &db::ArtifactVersionInsert {
                artifact_id: &artifact_id,
                version_label: body.version_label.as_deref(),
                parent_version_id: body.parent_version_id.as_deref(),
                body_format: &body_format_for_db,
                body: body.body.as_deref(),
                structured_payload: body.structured_payload.as_ref(),
                source_format: body.source_format.as_deref(),
                created_by_actor_id: &actor.actor_id,
                created_via_workflow_run_id: context_for_db.workflow_run_id.as_deref(),
                version_state: &version_state,
                idempotency_key: Some(&context_for_db.idempotency_key),
            },
        )?;
        complete_resumed_run(
            &conn,
            run.as_ref(),
            None,
            Some(result.record.artifact_version_id.clone()),
            None,
        )?;
        Ok::<_, anyhow::Error>((actor, result))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    emit_artifact_metric(
        "gateway_artifact_version_writes_total",
        &[
            ("project", ident_for_metric.as_str()),
            ("kind", "artifact_version"),
            ("result", result_label(write.replayed)),
        ],
    );
    emit_artifact_metric(
        "gateway_artifact_version_body_bytes",
        &[
            ("project", ident_for_metric.as_str()),
            ("kind", body_format_for_metric.as_str()),
        ],
    );
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "artifact_version_id": write.record.artifact_version_id,
            "workflow_run_id": workflow_run_id_for_response,
        }),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn get_artifact_version_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, version_id)): Path<(String, String, String)>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<VersionReadModel>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let data = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let version = db::get_artifact_version(&conn, &ident, &artifact_id, &version_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact_version not found"))?;
        let chunks = db::list_artifact_chunks(
            &conn,
            &ident,
            &artifact_id,
            &db::ArtifactChunkFilters {
                include_superseded: true,
                artifact_version_id: Some(&version_id),
                ..Default::default()
            },
        )?;
        Ok::<_, anyhow::Error>(ArtifactReadResponse {
            data: version_status(&artifact, version, &chunks),
            chunking_status: chunking_status_for(&artifact, &chunks),
        })
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(data))
}

pub async fn diff_artifact_version_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, version_id)): Path<(String, String, String)>,
    Query(query): Query<DiffQuery>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<ArtifactDiffResponse>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let data = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let target = db::get_artifact_version(&conn, &ident, &artifact_id, &version_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact_version not found"))?;
        let base_id = query
            .base_version_id
            .as_deref()
            .or(target.parent_version_id.as_deref());
        let base = base_id
            .map(|id| db::get_artifact_version(&conn, &ident, &artifact_id, id))
            .transpose()?
            .flatten();
        if base_id.is_some() && base.is_none() {
            anyhow::bail!("base artifact_version not found");
        }
        let from_text = base.as_ref().map(body_text).unwrap_or_default();
        let to_text = body_text(&target);
        let chunks = db::list_artifact_chunks(
            &conn,
            &ident,
            &artifact_id,
            &db::ArtifactChunkFilters {
                include_superseded: true,
                artifact_version_id: Some(&version_id),
                ..Default::default()
            },
        )?;
        emit_artifact_metric(
            "gateway_artifact_version_diff_bytes",
            &[
                ("project", ident.as_str()),
                ("kind", target.body_format.as_str()),
            ],
        );
        Ok::<_, anyhow::Error>(ArtifactReadResponse {
            data: ArtifactDiffResponse {
                from_version_id: base.map(|v| v.artifact_version_id),
                to_version_id: target.artifact_version_id,
                format: "unified",
                byte_delta: to_text.len() as isize - from_text.len() as isize,
                diff: simple_diff(&from_text, &to_text),
                chunking_status: chunking_status_for(&artifact, &chunks),
            },
            chunking_status: chunking_status_for(&artifact, &chunks),
        })
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(data))
}

pub async fn list_artifact_comments_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<Vec<db::ArtifactComment>>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let comments = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_artifact_comments(&conn, &ident, &artifact_id)
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: comments,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn create_artifact_comment_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<CreateCommentRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["comment.write"])?;
    let target_kind = trim_required(&body.target_kind, "target_kind")?;
    let target_id = trim_required(&body.target_id, "target_id")?;
    let comment_body = trim_required(&body.body, "body")?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let ident_for_metric = ident.clone();
    let target_kind_for_db = target_kind.clone();
    let target_kind_for_metric = target_kind.clone();
    let context_for_db = context.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let _ = validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let result = db::add_artifact_comment(
            &conn,
            &envelope,
            &db::ArtifactCommentInsert {
                artifact_id: &artifact_id,
                target_kind: &target_kind_for_db,
                target_id: &target_id,
                child_address: body.child_address.as_deref(),
                parent_comment_id: body.parent_comment_id.as_deref(),
                actor_id: &actor.actor_id,
                body: &comment_body,
                idempotency_key: Some(&context_for_db.idempotency_key),
            },
        )?;
        Ok::<_, anyhow::Error>((actor, result))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    emit_artifact_metric(
        "gateway_comment_writes_total",
        &[
            ("project", ident_for_metric.as_str()),
            ("target_kind", target_kind_for_metric.as_str()),
            ("result", result_label(write.replayed)),
        ],
    );
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated_resource("comment_id", write.record.comment_id.clone()),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn resolve_artifact_comment_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, comment_id)): Path<(String, String, String)>,
    headers: HeaderMap,
    Json(body): Json<ResolveCommentRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["comment.resolve"])?;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, comment) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let comment = db::resolve_artifact_comment(
            &conn,
            &ident,
            &artifact_id,
            &comment_id,
            &actor.actor_id,
            context_for_db.workflow_run_id.as_deref(),
            body.resolution_note.as_deref(),
        )?
        .ok_or_else(|| anyhow::anyhow!("comment not found"))?;
        Ok::<_, anyhow::Error>((actor, comment))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_ok_mutation(
        actor,
        &context,
        authorization,
        generated_resource("comment_id", comment.comment_id.clone()),
        Vec::new(),
        comment,
    ))
}

pub async fn reopen_artifact_comment_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, comment_id)): Path<(String, String, String)>,
    headers: HeaderMap,
    Json(body): Json<ReopenCommentRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["comment.write"])?;
    let note_body = body
        .note_body
        .unwrap_or_else(|| "reopened comment".to_string());
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, comment) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let comment = db::reopen_artifact_comment(
            &conn,
            &envelope,
            &ident,
            &artifact_id,
            &comment_id,
            &actor.actor_id,
            &note_body,
            Some(&context_for_db.idempotency_key),
        )?
        .ok_or_else(|| anyhow::anyhow!("comment not found"))?;
        Ok::<_, anyhow::Error>((actor, comment))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_ok_mutation(
        actor,
        &context,
        authorization,
        generated_resource("comment_id", comment.comment_id.clone()),
        Vec::new(),
        comment,
    ))
}

pub async fn list_artifact_contributions_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<Vec<db::ArtifactContribution>>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let contributions = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_artifact_contributions(&conn, &ident, &artifact_id)
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: contributions,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn create_artifact_contribution_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<CreateContributionRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["contribution.write"])?;
    let target_kind = trim_required(&body.target_kind, "target_kind")?;
    let target_id = trim_required(&body.target_id, "target_id")?;
    let contribution_kind = trim_required(&body.contribution_kind, "contribution_kind")?;
    validate_read_set_required(
        body.phase.as_deref(),
        &contribution_kind,
        body.read_set.as_ref(),
    )?;
    let role = trim_required(&body.role, "role")?;
    let body_format = body
        .body_format
        .as_deref()
        .unwrap_or("markdown")
        .to_string();
    validate_body_format(&body_format)?;
    let contribution_body = trim_required(&body.body, "body")?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let ident_for_metric = ident.clone();
    let contribution_kind_for_db = contribution_kind.clone();
    let contribution_kind_for_metric = contribution_kind.clone();
    let phase_for_metric = body.phase.clone();
    let context_for_db = context.clone();
    let workflow_run_id_for_response = context.workflow_run_id.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let run = validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let result = db::add_artifact_contribution(
            &conn,
            &envelope,
            &db::ArtifactContributionInsert {
                artifact_id: &artifact_id,
                target_kind: &target_kind,
                target_id: &target_id,
                contribution_kind: &contribution_kind_for_db,
                phase: body.phase.as_deref(),
                role: &role,
                actor_id: &actor.actor_id,
                workflow_run_id: context_for_db.workflow_run_id.as_deref(),
                read_set: body.read_set.as_ref(),
                body_format: &body_format,
                body: &contribution_body,
                idempotency_key: Some(&context_for_db.idempotency_key),
            },
        )?;
        complete_resumed_run(
            &conn,
            run.as_ref(),
            Some(result.record.contribution_id.clone()),
            None,
            None,
        )?;
        Ok::<_, anyhow::Error>((actor, result))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    emit_artifact_metric(
        "gateway_contribution_writes_total",
        &[
            ("project", ident_for_metric.as_str()),
            ("kind", contribution_kind_for_metric.as_str()),
            ("phase", phase_for_metric.as_deref().unwrap_or("none")),
            ("result", result_label(write.replayed)),
        ],
    );
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "contribution_id": write.record.contribution_id,
            "workflow_run_id": workflow_run_id_for_response,
        }),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn list_artifact_links_handler(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    Query(query): Query<ListLinksQuery>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<Vec<db::ArtifactLink>>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let links = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_artifact_links(
            &conn,
            &ident,
            &db::ArtifactLinkFilters {
                link_type: query.link_type.as_deref(),
                source_kind: query.source_kind.as_deref(),
                source_id: query.source_id.as_deref(),
                target_kind: query.target_kind.as_deref(),
                target_id: query.target_id.as_deref(),
            },
        )
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: links,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn create_artifact_link_handler(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    headers: HeaderMap,
    Json(body): Json<CreateLinkRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["link.write"])?;
    let link_type = trim_required(&body.link_type, "link_type")?;
    let source_kind = trim_required(&body.source_kind, "source_kind")?;
    let source_id = trim_required(&body.source_id, "source_id")?;
    let target_kind = trim_required(&body.target_kind, "target_kind")?;
    let target_id = trim_required(&body.target_id, "target_id")?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let ident_for_metric = ident.clone();
    let link_type_for_db = link_type.clone();
    let link_type_for_metric = link_type.clone();
    let context_for_db = context.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_project(&conn, &ident)?.ok_or_else(|| anyhow::anyhow!("project not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        // Links are project-scoped: the workflow run (if supplied) does NOT
        // need to belong to a specific artifact, but it must still be
        // retryable per the shared `ensure_workflow_run_retryable` semantics.
        let run_artifact =
            load_workflow_run_for_link(&conn, context_for_db.workflow_run_id.as_deref())?;
        let result = db::create_artifact_link(
            &conn,
            &envelope,
            &db::ArtifactLinkInsert {
                link_type: &link_type_for_db,
                source_kind: &source_kind,
                source_id: &source_id,
                source_version_id: body.source_version_id.as_deref(),
                source_child_address: body.source_child_address.as_deref(),
                target_kind: &target_kind,
                target_id: &target_id,
                target_version_id: body.target_version_id.as_deref(),
                target_child_address: body.target_child_address.as_deref(),
                created_by_actor_id: &actor.actor_id,
                created_via_workflow_run_id: context_for_db.workflow_run_id.as_deref(),
                idempotency_key: Some(&context_for_db.idempotency_key),
                supersedes_link_id: body.supersedes_link_id.as_deref(),
            },
        )?;
        complete_resumed_run(
            &conn,
            run_artifact.as_ref(),
            None,
            None,
            Some(result.record.link_id.clone()),
        )?;
        Ok::<_, anyhow::Error>((actor, result))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    emit_artifact_metric(
        "gateway_link_writes_total",
        &[
            ("project", ident_for_metric.as_str()),
            ("link_type", link_type_for_metric.as_str()),
            ("result", result_label(write.replayed)),
        ],
    );
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated_resource("link_id", write.record.link_id.clone()),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn list_artifact_workflow_runs_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<Vec<db::WorkflowRun>>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let runs = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_workflow_runs(&conn, &ident, &artifact_id)
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: runs,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn start_artifact_workflow_run_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<StartWorkflowRunRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["workflow_run.start"])?;
    let workflow_kind = trim_required(&body.workflow_kind, "workflow_kind")?;
    if !matches!(
        workflow_kind.as_str(),
        "design_review_round" | "spec_iteration" | "spec_acceptance" | "spec_task_generation"
    ) {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "invalid_workflow_kind".into(),
        ));
    }
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let ident_for_metric = ident.clone();
    let workflow_kind_for_db = workflow_kind.clone();
    let workflow_kind_for_metric = workflow_kind.clone();
    let context_for_db = context.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let participants = body.participant_actor_ids.unwrap_or_default();
        let result = db::start_workflow_run(
            &conn,
            &envelope,
            &db::WorkflowRunInsert {
                artifact_id: &artifact_id,
                workflow_kind: &workflow_kind_for_db,
                phase: body.phase.as_deref(),
                round_id: body.round_id.as_deref(),
                coordinator_actor_id: &actor.actor_id,
                participant_actor_ids: &participants,
                source_artifact_version_id: body.source_artifact_version_id.as_deref(),
                read_set: body.read_set.as_ref(),
                idempotency_key: Some(&context_for_db.idempotency_key),
                is_resumable: body.is_resumable.unwrap_or(matches!(
                    workflow_kind_for_db.as_str(),
                    "spec_task_generation"
                )),
            },
        )?;
        Ok::<_, anyhow::Error>((actor, result))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    emit_artifact_metric(
        "gateway_workflow_runs_total",
        &[
            ("project", ident_for_metric.as_str()),
            ("kind", workflow_kind_for_metric.as_str()),
            ("state", write.record.state.as_str()),
        ],
    );
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated_resource("workflow_run_id", write.record.workflow_run_id.clone()),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn get_artifact_workflow_run_handler(
    State(state): State<AppState>,
    Path((ident, _artifact_id, workflow_run_id)): Path<(String, String, String)>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<db::WorkflowRun>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let run = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_workflow_run(&conn, &workflow_run_id)?
            .ok_or_else(|| anyhow::anyhow!("workflow_run not found"))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: run,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn complete_artifact_workflow_run_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, workflow_run_id)): Path<(String, String, String)>,
    headers: HeaderMap,
    Json(body): Json<CompleteWorkflowRunRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["workflow_run.complete"])?;
    let state_value = trim_required(&body.state, "state")?;
    if !matches!(state_value.as_str(), "succeeded" | "failed" | "cancelled") {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "invalid_workflow_run_state".to_string(),
        ));
    }
    let db = state.db.clone();
    let ident_for_metric = ident.clone();
    let context_for_db = context.clone();
    let (actor, updated) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let current = db::get_workflow_run(&conn, &workflow_run_id)?
            .ok_or_else(|| anyhow::anyhow!("workflow_run not found"))?;
        if current.artifact_id != artifact_id {
            anyhow::bail!("workflow_run does not belong to artifact");
        }
        if current.coordinator_actor_id != actor.actor_id {
            anyhow::bail!("workflow_run coordinator mismatch");
        }
        let updated = db::update_workflow_run(
            &conn,
            &workflow_run_id,
            &db::WorkflowRunUpdate {
                state: Some(&state_value),
                failure_reason: if state_value == "failed" {
                    Some(body.failure_reason.as_deref())
                } else {
                    Some(None)
                },
                generated_contribution_ids: body.generated_contribution_ids.as_deref(),
                generated_version_ids: body.generated_version_ids.as_deref(),
                generated_task_ids: body.generated_task_ids.as_deref(),
                generated_link_ids: body.generated_link_ids.as_deref(),
                generated_chunk_ids: body.generated_chunk_ids.as_deref(),
                ended_at: None,
            },
        )?
        .ok_or_else(|| anyhow::anyhow!("workflow_run not found"))?;
        Ok::<_, anyhow::Error>((actor, updated))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    emit_artifact_metric(
        "gateway_workflow_runs_total",
        &[
            ("project", ident_for_metric.as_str()),
            ("kind", updated.workflow_kind.as_str()),
            ("state", updated.state.as_str()),
        ],
    );
    let generated = generated_workflow_resources(&updated);
    Ok(finalize_ok_mutation(
        actor,
        &context,
        authorization,
        generated,
        Vec::new(),
        updated,
    ))
}

pub async fn create_design_review_handler(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    headers: HeaderMap,
    Json(body): Json<DesignReviewCreateRequest>,
) -> Result<Response> {
    let (context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec!["artifact.write", "artifact_version.create"],
    )?;
    let title = trim_required(&body.title, "title")?;
    let labels = body.labels.unwrap_or_default();
    let body_format = body
        .body_format
        .as_deref()
        .unwrap_or("markdown")
        .to_string();
    validate_body_format(&body_format)?;
    let payload = design_review_payload(
        body.source_artifact_id.clone(),
        body.source_artifact_version_id.clone(),
    );
    artifact_body_schema_allowed(&state, &body_format, Some(&payload))?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, artifact_write, version_write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_project(&conn, &ident)?.ok_or_else(|| anyhow::anyhow!("project not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let artifact_write = db::create_artifact(
            &conn,
            &envelope,
            &db::ArtifactInsert {
                project_ident: &ident,
                kind: "design_review",
                subkind: None,
                title: &title,
                labels: &labels,
                created_by_actor_id: &actor.actor_id,
            },
        )?;
        let version_write = if body.body.is_some() {
            let version_key = format!("{}:initial-version", context_for_db.idempotency_key);
            Some(db::create_artifact_version(
                &conn,
                &envelope,
                &db::ArtifactVersionInsert {
                    artifact_id: &artifact_write.record.artifact_id,
                    version_label: Some("source"),
                    parent_version_id: None,
                    body_format: &body_format,
                    body: body.body.as_deref(),
                    structured_payload: Some(&payload),
                    source_format: None,
                    created_by_actor_id: &actor.actor_id,
                    created_via_workflow_run_id: None,
                    version_state: "under_review",
                    idempotency_key: Some(&version_key),
                },
            )?)
        } else {
            None
        };
        Ok::<_, anyhow::Error>((actor, artifact_write, version_write))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    let mut warnings = artifact_write.warnings;
    if let Some(version) = &version_write {
        warnings.extend(version.warnings.clone());
    }
    let generated = serde_json::json!({
        "artifact_id": artifact_write.record.artifact_id,
        "artifact_version_id": version_write.as_ref().map(|write| write.record.artifact_version_id.clone()),
    });
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated,
        warnings,
        artifact_write.replayed && version_write.as_ref().is_some_and(|write| write.replayed),
        serde_json::json!({
            "artifact": artifact_write.record,
            "version": version_write.map(|write| write.record),
        }),
    ))
}

pub async fn create_design_review_round_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<DesignReviewRoundRequest>,
) -> Result<Response> {
    let (context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec!["workflow_run.start", "artifact.write"],
    )?;
    let source_version_id = trim_required(
        &body.source_artifact_version_id,
        "source_artifact_version_id",
    )?;
    let round_id = body
        .round_id
        .filter(|v| !v.trim().is_empty())
        .unwrap_or_else(|| format!("review-round:{}", context.idempotency_key));
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, write, artifact) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if artifact.kind != "design_review" {
            anyhow::bail!("artifact is not a design_review");
        }
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let participants = body.participant_actor_ids.unwrap_or_default();
        let write = db::start_workflow_run(
            &conn,
            &envelope,
            &db::WorkflowRunInsert {
                artifact_id: &artifact_id,
                workflow_kind: "design_review_round",
                phase: Some("pass_1"),
                round_id: Some(&round_id),
                coordinator_actor_id: &actor.actor_id,
                participant_actor_ids: &participants,
                source_artifact_version_id: Some(&source_version_id),
                read_set: body.read_set.as_ref(),
                idempotency_key: Some(&context_for_db.idempotency_key),
                is_resumable: false,
            },
        )?;
        let artifact = db::update_artifact(
            &conn,
            &ident,
            &artifact_id,
            &db::ArtifactUpdate {
                lifecycle_state: Some("active"),
                review_state: Some("collecting_reviews"),
                ..Default::default()
            },
            &envelope,
        )?
        .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        Ok::<_, anyhow::Error>((actor, write, artifact))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated_resource("workflow_run_id", write.record.workflow_run_id.clone()),
        write.warnings,
        write.replayed,
        serde_json::json!({
            "workflow_run": write.record,
            "artifact": artifact,
        }),
    ))
}

pub async fn create_design_review_contribution_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, workflow_run_id)): Path<(String, String, String)>,
    headers: HeaderMap,
    Json(body): Json<DesignReviewContributionRequest>,
) -> Result<Response> {
    let (mut context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["contribution.write"])?;
    context.workflow_run_id = Some(workflow_run_id.clone());
    let phase = trim_required(&body.phase, "phase")?;
    let role = body.role.unwrap_or_else(|| "reviewer".to_string());
    let role = trim_required(&role, "role")?;
    let body_format = body
        .body_format
        .as_deref()
        .unwrap_or("markdown")
        .to_string();
    validate_body_format(&body_format)?;
    let contribution_body = trim_required(&body.body, "body")?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let workflow_run_id_for_response = workflow_run_id.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if artifact.kind != "design_review" {
            anyhow::bail!("artifact is not a design_review");
        }
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let run = validate_workflow_run_for_mutation(&conn, &artifact_id, Some(&workflow_run_id))?
            .ok_or_else(|| anyhow::anyhow!("workflow run not found"))?;
        if run.workflow_kind != "design_review_round" {
            anyhow::bail!("workflow run is not a design_review_round");
        }
        let reviewed_version_id = body
            .reviewed_version_id
            .as_deref()
            .or(run.source_artifact_version_id.as_deref())
            .ok_or_else(|| anyhow::anyhow!("reviewed version required"))?
            .to_string();
        let read_set = body
            .read_set
            .unwrap_or_else(|| read_set_for_reviewed_version(&reviewed_version_id));
        validate_design_review_phase(&phase, Some(&read_set)).map_err(|e| anyhow::anyhow!(e.1))?;
        let write = db::add_artifact_contribution(
            &conn,
            &envelope,
            &db::ArtifactContributionInsert {
                artifact_id: &artifact_id,
                target_kind: "artifact_version",
                target_id: &reviewed_version_id,
                contribution_kind: "review",
                phase: Some(&phase),
                role: &role,
                actor_id: &actor.actor_id,
                workflow_run_id: Some(&workflow_run_id),
                read_set: Some(&read_set),
                body_format: &body_format,
                body: &contribution_body,
                idempotency_key: Some(&context_for_db.idempotency_key),
            },
        )?;
        db::append_workflow_run_outputs(
            &conn,
            &workflow_run_id,
            Some(&write.record.contribution_id),
            None,
        )?;
        Ok::<_, anyhow::Error>((actor, write))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "contribution_id": write.record.contribution_id,
            "workflow_run_id": workflow_run_id_for_response,
        }),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn create_design_review_synthesis_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, workflow_run_id)): Path<(String, String, String)>,
    headers: HeaderMap,
    Json(body): Json<DesignReviewSynthesisRequest>,
) -> Result<Response> {
    let (mut context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec![
            "contribution.write",
            "artifact_version.create",
            "artifact.write",
        ],
    )?;
    context.workflow_run_id = Some(workflow_run_id.clone());
    validate_synthesis_read_set(&body.read_set)?;
    let body_format = body
        .body_format
        .as_deref()
        .unwrap_or("markdown")
        .to_string();
    validate_body_format(&body_format)?;
    let synthesis_body = trim_required(&body.body, "body")?;
    let should_create_version = body.create_version.unwrap_or(true);
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let workflow_run_id_for_response = workflow_run_id.clone();
    let (actor, contribution_write, version_write, artifact) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if artifact.kind != "design_review" {
            anyhow::bail!("artifact is not a design_review");
        }
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let run = validate_workflow_run_for_mutation(&conn, &artifact_id, Some(&workflow_run_id))?
            .ok_or_else(|| anyhow::anyhow!("workflow run not found"))?;
        if run.workflow_kind != "design_review_round" {
            anyhow::bail!("workflow run is not a design_review_round");
        }
        let reviewed_version_id = body
            .reviewed_version_id
            .as_deref()
            .or(run.source_artifact_version_id.as_deref())
            .ok_or_else(|| anyhow::anyhow!("reviewed version required"))?
            .to_string();
        let contribution_write = db::add_artifact_contribution(
            &conn,
            &envelope,
            &db::ArtifactContributionInsert {
                artifact_id: &artifact_id,
                target_kind: "artifact_version",
                target_id: &reviewed_version_id,
                contribution_kind: "synthesis",
                phase: Some("synthesis"),
                role: "analyst",
                actor_id: &actor.actor_id,
                workflow_run_id: Some(&workflow_run_id),
                read_set: Some(&body.read_set),
                body_format: &body_format,
                body: &synthesis_body,
                idempotency_key: Some(&context_for_db.idempotency_key),
            },
        )?;
        let version_write = if should_create_version {
            let version_key = format!("{}:synthesis-version", context_for_db.idempotency_key);
            let payload = serde_json::json!({
                "workflow": "design_review_synthesis",
                "round_id": run.round_id,
                "source_artifact_version_id": reviewed_version_id,
                "synthesis_contribution_id": contribution_write.record.contribution_id,
                "read_set": body.read_set,
            });
            Some(db::create_artifact_version(
                &conn,
                &envelope,
                &db::ArtifactVersionInsert {
                    artifact_id: &artifact_id,
                    version_label: body.version_label.as_deref(),
                    parent_version_id: Some(&reviewed_version_id),
                    body_format: &body_format,
                    body: Some(&synthesis_body),
                    structured_payload: Some(&payload),
                    source_format: None,
                    created_by_actor_id: &actor.actor_id,
                    created_via_workflow_run_id: Some(&workflow_run_id),
                    version_state: "draft",
                    idempotency_key: Some(&version_key),
                },
            )?)
        } else {
            None
        };
        db::append_workflow_run_outputs(
            &conn,
            &workflow_run_id,
            Some(&contribution_write.record.contribution_id),
            version_write
                .as_ref()
                .map(|write| write.record.artifact_version_id.as_str()),
        )?;
        let artifact = db::update_artifact(
            &conn,
            &ident,
            &artifact_id,
            &db::ArtifactUpdate {
                review_state: Some("needs_user_decision"),
                ..Default::default()
            },
            &envelope,
        )?
        .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        Ok::<_, anyhow::Error>((actor, contribution_write, version_write, artifact))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    let mut warnings = contribution_write.warnings;
    if let Some(version) = &version_write {
        warnings.extend(version.warnings.clone());
    }
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "contribution_id": contribution_write.record.contribution_id,
            "artifact_version_id": version_write.as_ref().map(|write| write.record.artifact_version_id.clone()),
            "workflow_run_id": workflow_run_id_for_response,
        }),
        warnings,
        contribution_write.replayed && version_write.as_ref().is_some_and(|write| write.replayed),
        serde_json::json!({
            "contribution": contribution_write.record,
            "version": version_write.map(|write| write.record),
            "artifact": artifact,
        }),
    ))
}

pub async fn update_design_review_state_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<DesignReviewStateRequest>,
) -> Result<Response> {
    let (context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec!["artifact.write", "contribution.write"],
    )?;
    if body.lifecycle_state.is_none() && body.review_state.is_none() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "state_required".to_string(),
        ));
    }
    let note = body
        .note
        .unwrap_or_else(|| "design review state transition".to_string());
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let workflow_run_id_for_response = context.workflow_run_id.clone();
    let (actor, artifact, contribution) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if artifact.kind != "design_review" {
            anyhow::bail!("artifact is not a design_review");
        }
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let run = validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let contribution = db::add_artifact_contribution(
            &conn,
            &envelope,
            &db::ArtifactContributionInsert {
                artifact_id: &artifact_id,
                target_kind: "artifact",
                target_id: &artifact_id,
                contribution_kind: "state_transition",
                phase: Some("state_transition"),
                role: "coordinator",
                actor_id: &actor.actor_id,
                workflow_run_id: context_for_db.workflow_run_id.as_deref(),
                read_set: None,
                body_format: "markdown",
                body: &note,
                idempotency_key: Some(&context_for_db.idempotency_key),
            },
        )?;
        if let Some(run) = run.as_ref() {
            db::append_workflow_run_outputs(
                &conn,
                &run.workflow_run_id,
                Some(&contribution.record.contribution_id),
                None,
            )?;
        }
        let artifact = db::update_artifact(
            &conn,
            &ident,
            &artifact_id,
            &db::ArtifactUpdate {
                lifecycle_state: body.lifecycle_state.as_deref(),
                review_state: body.review_state.as_deref(),
                ..Default::default()
            },
            &envelope,
        )?
        .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        Ok::<_, anyhow::Error>((actor, artifact, contribution))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "artifact_id": artifact.artifact_id,
            "contribution_id": contribution.record.contribution_id,
            "workflow_run_id": workflow_run_id_for_response,
        }),
        contribution.warnings,
        contribution.replayed,
        serde_json::json!({
            "artifact": artifact,
            "contribution": contribution.record,
        }),
    ))
}

pub async fn list_design_review_contributions_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    Query(query): Query<ListDesignReviewContributionsQuery>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<Vec<db::ArtifactContribution>>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let contributions = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_design_review_contributions(
            &conn,
            &ident,
            &artifact_id,
            &db::DesignReviewContributionFilters {
                round_id: query.round_id.as_deref(),
                phase: query.phase.as_deref(),
                role: query.role.as_deref(),
                reviewed_version_id: query.reviewed_version_id.as_deref(),
                read_set_contains: query.read_set_contains.as_deref(),
            },
        )
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: contributions,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn create_spec_handler(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    headers: HeaderMap,
    Json(body): Json<SpecImportRequest>,
) -> Result<Response> {
    let (context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec!["artifact.write", "artifact_version.create"],
    )?;
    let title = trim_required(&body.title, "title")?;
    let labels = body.labels.unwrap_or_default();
    let payload = spec_structured_payload(
        body.manifest,
        body.file_bodies,
        body.source_doc,
        body.source_artifact_id,
        body.source_artifact_version_id,
    )?;
    artifact_body_schema_allowed(&state, "markdown", Some(&payload))?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, artifact_write, version_write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_project(&conn, &ident)?.ok_or_else(|| anyhow::anyhow!("project not found"))?;
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let artifact_write = db::create_artifact(
            &conn,
            &envelope,
            &db::ArtifactInsert {
                project_ident: &ident,
                kind: "spec",
                subkind: Some("implementation"),
                title: &title,
                labels: &labels,
                created_by_actor_id: &actor.actor_id,
            },
        )?;
        let version_key = format!("{}:initial-version", context_for_db.idempotency_key);
        let version_write = db::create_artifact_version(
            &conn,
            &envelope,
            &db::ArtifactVersionInsert {
                artifact_id: &artifact_write.record.artifact_id,
                version_label: Some("imported"),
                parent_version_id: None,
                body_format: "markdown",
                body: body.body.as_deref(),
                structured_payload: Some(&payload),
                source_format: Some("gateway-spec-directory"),
                created_by_actor_id: &actor.actor_id,
                created_via_workflow_run_id: None,
                version_state: "draft",
                idempotency_key: Some(&version_key),
            },
        )?;
        Ok::<_, anyhow::Error>((actor, artifact_write, version_write))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    let generated = serde_json::json!({
        "artifact_id": artifact_write.record.artifact_id,
        "artifact_version_id": version_write.record.artifact_version_id
    });
    let mut warnings = artifact_write.warnings;
    warnings.extend(version_write.warnings);
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated,
        warnings,
        artifact_write.replayed && version_write.replayed,
        serde_json::json!({
            "artifact": artifact_write.record,
            "version": version_write.record,
        }),
    ))
}

pub async fn create_spec_version_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<SpecVersionRequest>,
) -> Result<Response> {
    let (context, authorization) =
        begin_artifact_mutation(&state, &headers, &ident, vec!["artifact_version.create"])?;
    let payload =
        spec_structured_payload(body.manifest, body.file_bodies, body.source_doc, None, None)?;
    artifact_body_schema_allowed(&state, "markdown", Some(&payload))?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let workflow_run_id_for_response = context.workflow_run_id.clone();
    let (actor, write) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if artifact.kind != "spec" {
            anyhow::bail!("artifact is not a spec");
        }
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let run = validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let result = db::create_artifact_version(
            &conn,
            &envelope,
            &db::ArtifactVersionInsert {
                artifact_id: &artifact_id,
                version_label: body.version_label.as_deref(),
                parent_version_id: body.parent_version_id.as_deref(),
                body_format: "markdown",
                body: body.body.as_deref(),
                structured_payload: Some(&payload),
                source_format: Some("gateway-spec-directory"),
                created_by_actor_id: &actor.actor_id,
                created_via_workflow_run_id: context_for_db.workflow_run_id.as_deref(),
                version_state: "draft",
                idempotency_key: Some(&context_for_db.idempotency_key),
            },
        )?;
        complete_resumed_run(
            &conn,
            run.as_ref(),
            None,
            Some(result.record.artifact_version_id.clone()),
            None,
        )?;
        Ok::<_, anyhow::Error>((actor, result))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "artifact_version_id": write.record.artifact_version_id,
            "workflow_run_id": workflow_run_id_for_response,
        }),
        write.warnings,
        write.replayed,
        write.record,
    ))
}

pub async fn accept_spec_version_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<SpecAcceptRequest>,
) -> Result<Response> {
    let (context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec!["artifact_version.accept", "artifact.write"],
    )?;
    let version_id = trim_required(&body.version_id, "version_id")?;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let version_id_for_response = version_id.clone();
    let (actor, contribution) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let artifact = db::get_artifact_summary(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if artifact.kind != "spec" {
            anyhow::bail!("artifact is not a spec");
        }
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        validate_workflow_run_for_mutation(
            &conn,
            &artifact_id,
            context_for_db.workflow_run_id.as_deref(),
        )?;
        let contribution = db::accept_artifact_version(
            &conn,
            &ident,
            &artifact_id,
            &version_id,
            &actor.actor_id,
            context_for_db.workflow_run_id.as_deref(),
            Some(&context_for_db.idempotency_key),
        )?;
        Ok::<_, anyhow::Error>((actor, contribution))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_ok_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "artifact_version_id": version_id_for_response,
            "contribution_id": contribution.contribution_id,
        }),
        Vec::new(),
        contribution,
    ))
}

pub async fn get_spec_manifest_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    Query(query): Query<SpecManifestQuery>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<SpecManifestResponse>>> {
    require_artifact_read(&state, &headers, &ident)?;
    let db = state.db.clone();
    let response = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let detail = db::get_artifact(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if detail.artifact.kind != "spec" {
            anyhow::bail!("artifact is not a spec");
        }
        let version = if let Some(version_id) = query.version_id {
            db::get_artifact_version(&conn, &ident, &artifact_id, &version_id)?
                .ok_or_else(|| anyhow::anyhow!("version not found"))?
        } else {
            detail
                .accepted_version
                .or(detail.current_version)
                .ok_or_else(|| anyhow::anyhow!("spec version not found"))?
        };
        spec_manifest_from_version(&artifact_id, &version).map_err(|err| anyhow::anyhow!(err.1))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(Json(ArtifactReadResponse {
        data: response,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

pub async fn get_spec_manifest_item_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id, manifest_item_id)): Path<(String, String, String)>,
    Query(query): Query<SpecManifestQuery>,
    headers: HeaderMap,
) -> Result<Json<ArtifactReadResponse<SpecManifestItem>>> {
    let manifest = get_spec_manifest_handler(
        State(state),
        Path((ident, artifact_id)),
        Query(query),
        headers,
    )
    .await?
    .0
    .data;
    let item = manifest
        .items
        .into_iter()
        .find(|item| item.manifest_item_id == manifest_item_id)
        .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "manifest_item not found".to_string()))?;
    Ok(Json(ArtifactReadResponse {
        data: item,
        chunking_status: ChunkingStatus {
            status: "none",
            current_chunk_count: 0,
            stale_chunk_count: 0,
            superseded_chunk_count: 0,
            failed_addresses: Vec::new(),
        },
    }))
}

#[allow(clippy::too_many_arguments)]
fn link_task_to_spec_item(
    conn: &rusqlite::Connection,
    envelope: &db::ArtifactOperationsEnvelope,
    actor_id: &str,
    workflow_run_id: &str,
    source_version_id: &str,
    manifest_item_id: &str,
    task_id: &str,
) -> anyhow::Result<db::ArtifactWriteResult<db::ArtifactLink>> {
    let child_address = format!("manifest.items[{manifest_item_id}]");
    db::create_artifact_link(
        conn,
        envelope,
        &db::ArtifactLinkInsert {
            link_type: "task_generated_from_spec",
            source_kind: "artifact_version",
            source_id: source_version_id,
            source_version_id: Some(source_version_id),
            source_child_address: Some(&child_address),
            target_kind: "task",
            target_id: task_id,
            target_version_id: Some(source_version_id),
            target_child_address: None,
            created_by_actor_id: actor_id,
            created_via_workflow_run_id: Some(workflow_run_id),
            idempotency_key: Some(&item_generation_key(source_version_id, manifest_item_id)),
            supersedes_link_id: None,
        },
    )
}

pub async fn generate_spec_tasks_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<GenerateSpecTasksRequest>,
) -> Result<Response> {
    let origin = task_creation_origin(&headers)?;
    let (context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec![
            "task.generate_from_spec",
            "workflow_run.start",
            "link.write",
        ],
    )?;
    if !body.confirmed {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "task_generation_confirmation_required".to_string(),
        ));
    }
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, output) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let detail = db::get_artifact(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if detail.artifact.kind != "spec" {
            anyhow::bail!("artifact is not a spec");
        }
        let accepted = detail
            .accepted_version
            .ok_or_else(|| anyhow::anyhow!("accepted spec version required"))?;
        let manifest = spec_manifest_from_version(&artifact_id, &accepted)
            .map_err(|err| anyhow::anyhow!(err.1))?;
        let selected: std::collections::BTreeSet<String> = body
            .manifest_item_ids
            .unwrap_or_default()
            .into_iter()
            .collect();
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let read_set = serde_json::json!({
            "artifact_version_ids": [accepted.artifact_version_id.clone()]
        });
        let run_write = db::start_workflow_run(
            &conn,
            &envelope,
            &db::WorkflowRunInsert {
                artifact_id: &artifact_id,
                workflow_kind: "spec_task_generation",
                phase: Some("generate_tasks"),
                round_id: None,
                coordinator_actor_id: &actor.actor_id,
                participant_actor_ids: &[],
                source_artifact_version_id: Some(&accepted.artifact_version_id),
                read_set: Some(&read_set),
                idempotency_key: Some(&context_for_db.idempotency_key),
                is_resumable: true,
            },
        )?;
        ensure_workflow_run_retryable(&run_write.record)?;

        let mut task_ids = run_write.record.generated_task_ids.clone();
        let mut link_ids = run_write.record.generated_link_ids.clone();
        let mut item_outputs = Vec::new();
        let reporter = body
            .reporter
            .as_deref()
            .unwrap_or(&context_for_db.actor.agent_id)
            .to_string();
        for item in manifest.items {
            if !selected.is_empty() && !selected.contains(&item.manifest_item_id) {
                continue;
            }
            let child_address = format!("manifest.items[{}]", item.manifest_item_id);
            let existing_link = db::list_artifact_links(
                &conn,
                &ident,
                &db::ArtifactLinkFilters {
                    link_type: Some("task_generated_from_spec"),
                    source_kind: Some("artifact_version"),
                    source_id: Some(&accepted.artifact_version_id),
                    target_kind: Some("task"),
                    target_id: None,
                },
            )?
            .into_iter()
            .find(|link| link.source_child_address.as_deref() == Some(child_address.as_str()));
            if let Some(link) = existing_link {
                if !task_ids.contains(&link.target_id) {
                    task_ids.push(link.target_id.clone());
                }
                if !link_ids.contains(&link.link_id) {
                    link_ids.push(link.link_id.clone());
                }
                item_outputs.push(serde_json::json!({
                    "manifest_item_id": item.manifest_item_id,
                    "task_id": link.target_id,
                    "link_id": link.link_id,
                    "reused": true
                }));
                continue;
            }

            let task = if let Some(task_id) = item.gateway_task_id.as_deref() {
                db::get_task_detail(&conn, &ident, task_id)?.map(|detail| detail.task)
            } else {
                db::find_task_by_spec_source(
                    &conn,
                    &ident,
                    &artifact_id,
                    &accepted.artifact_version_id,
                    &item.manifest_item_id,
                )?
            };
            let task = match task {
                Some(task) => task,
                None => {
                    let mut labels = item.labels.clone();
                    if !labels.iter().any(|label| label == "generated-from-spec") {
                        labels.push("generated-from-spec".to_string());
                    }
                    if let Some(team) = &item.team {
                        labels.push(team.clone());
                    }
                    crate::task_events::mutation(&conn, origin.as_ref(), |conn| db::insert_task(
                        conn,
                        &ident,
                        &generated_spec_task_title(&item),
                        Some(&format!(
                            "Generated from spec artifact {} version {} manifest item {}.",
                            artifact_id, accepted.artifact_version_id, item.manifest_item_id
                        )),
                        Some(&generated_task_specification(
                            &artifact_id,
                            &accepted.artifact_version_id,
                            &item,
                        )),
                        &labels,
                        body.hostname.as_deref(),
                        &reporter,
                    ))?
                }
            };
            let link = link_task_to_spec_item(
                &conn,
                &envelope,
                &actor.actor_id,
                &run_write.record.workflow_run_id,
                &accepted.artifact_version_id,
                &item.manifest_item_id,
                &task.id,
            )?;
            crate::task_events::mutation(&conn, origin.as_ref(), |conn| db::insert_comment(
                conn,
                &task.id,
                "system",
                "system",
                &format!(
                    "Generated from spec artifact `{}` accepted version `{}` manifest item `{}`. Compatibility fallback: the current task schema has no dedicated source fields, so the source tuple is embedded in this task's specification/details and mirrored by artifact link `{}`.",
                    artifact_id,
                    accepted.artifact_version_id,
                    item.manifest_item_id,
                    link.record.link_id
                ),
            ))?;
            if !task_ids.contains(&task.id) {
                task_ids.push(task.id.clone());
            }
            if !link_ids.contains(&link.record.link_id) {
                link_ids.push(link.record.link_id.clone());
            }
            item_outputs.push(serde_json::json!({
                "manifest_item_id": item.manifest_item_id,
                "task_id": task.id,
                "link_id": link.record.link_id,
                "reused": false
            }));
        }
        let updated_run = db::update_workflow_run(
            &conn,
            &run_write.record.workflow_run_id,
            &db::WorkflowRunUpdate {
                state: Some("succeeded"),
                failure_reason: Some(None),
                generated_contribution_ids: Some(&run_write.record.generated_contribution_ids),
                generated_version_ids: Some(&run_write.record.generated_version_ids),
                generated_task_ids: Some(&task_ids),
                generated_link_ids: Some(&link_ids),
                generated_chunk_ids: Some(&run_write.record.generated_chunk_ids),
                ended_at: None,
            },
        )?
        .ok_or_else(|| anyhow::anyhow!("workflow_run not found"))?;
        Ok::<_, anyhow::Error>((
            actor,
            GenerateSpecTasksResponse {
                artifact_id,
                artifact_version_id: accepted.artifact_version_id,
                workflow_run_id: updated_run.workflow_run_id,
                generated_task_ids: task_ids,
                generated_link_ids: link_ids,
                items: item_outputs,
                replayed: run_write.replayed,
            },
        ))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        serde_json::json!({
            "workflow_run_id": output.workflow_run_id,
            "generated_task_ids": output.generated_task_ids.clone(),
            "generated_link_ids": output.generated_link_ids.clone(),
        }),
        Vec::new(),
        output.replayed,
        output,
    ))
}

pub async fn link_existing_spec_task_handler(
    State(state): State<AppState>,
    Path((ident, artifact_id)): Path<(String, String)>,
    headers: HeaderMap,
    Json(body): Json<LinkSpecTaskRequest>,
) -> Result<Response> {
    let (context, authorization) = begin_artifact_mutation(
        &state,
        &headers,
        &ident,
        vec![
            "task.generate_from_spec",
            "workflow_run.start",
            "link.write",
        ],
    )?;
    let manifest_item_id = trim_required(&body.manifest_item_id, "manifest_item_id")?;
    let task_id = trim_required(&body.task_id, "task_id")?;
    let envelope = state.artifact_operations;
    let db = state.db.clone();
    let context_for_db = context.clone();
    let (actor, link) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let detail = db::get_artifact(&conn, &ident, &artifact_id)?
            .ok_or_else(|| anyhow::anyhow!("artifact not found"))?;
        if detail.artifact.kind != "spec" {
            anyhow::bail!("artifact is not a spec");
        }
        db::get_task_detail(&conn, &ident, &task_id)?
            .ok_or_else(|| anyhow::anyhow!("task not found"))?;
        let version = if let Some(version_id) = body.version_id {
            db::get_artifact_version(&conn, &ident, &artifact_id, &version_id)?
                .ok_or_else(|| anyhow::anyhow!("version not found"))?
        } else {
            detail
                .accepted_version
                .ok_or_else(|| anyhow::anyhow!("accepted spec version required"))?
        };
        let manifest = spec_manifest_from_version(&artifact_id, &version)
            .map_err(|err| anyhow::anyhow!(err.1))?;
        if !manifest
            .items
            .iter()
            .any(|item| item.manifest_item_id == manifest_item_id)
        {
            anyhow::bail!("manifest_item not found");
        }
        let actor = resolve_artifact_actor(
            &conn,
            &context_for_db.actor,
            body.actor_display_name.as_deref(),
        )?;
        let read_set = serde_json::json!({
            "artifact_version_ids": [version.artifact_version_id.clone()]
        });
        let run_write = db::start_workflow_run(
            &conn,
            &envelope,
            &db::WorkflowRunInsert {
                artifact_id: &artifact_id,
                workflow_kind: "spec_task_generation",
                phase: Some("link_existing_task"),
                round_id: None,
                coordinator_actor_id: &actor.actor_id,
                participant_actor_ids: &[],
                source_artifact_version_id: Some(&version.artifact_version_id),
                read_set: Some(&read_set),
                idempotency_key: Some(&context_for_db.idempotency_key),
                is_resumable: true,
            },
        )?;
        ensure_workflow_run_retryable(&run_write.record)?;
        let link = link_task_to_spec_item(
            &conn,
            &envelope,
            &actor.actor_id,
            &run_write.record.workflow_run_id,
            &version.artifact_version_id,
            &manifest_item_id,
            &task_id,
        )?;
        let mut task_ids = run_write.record.generated_task_ids;
        let mut link_ids = run_write.record.generated_link_ids;
        if !task_ids.contains(&task_id) {
            task_ids.push(task_id.clone());
        }
        if !link_ids.contains(&link.record.link_id) {
            link_ids.push(link.record.link_id.clone());
        }
        db::update_workflow_run(
            &conn,
            &run_write.record.workflow_run_id,
            &db::WorkflowRunUpdate {
                state: Some("succeeded"),
                failure_reason: Some(None),
                generated_contribution_ids: Some(&run_write.record.generated_contribution_ids),
                generated_version_ids: Some(&run_write.record.generated_version_ids),
                generated_task_ids: Some(&task_ids),
                generated_link_ids: Some(&link_ids),
                generated_chunk_ids: Some(&run_write.record.generated_chunk_ids),
                ended_at: None,
            },
        )?;
        Ok::<_, anyhow::Error>((actor, link))
    })
    .await
    .map_err(AppError::from)?
    .map_err(map_db_error)?;
    Ok(finalize_mutation(
        actor,
        &context,
        authorization,
        generated_resource("link_id", link.record.link_id.clone()),
        link.warnings,
        link.replayed,
        link.record,
    ))
}

// ── Theme (GET/POST /theme) ──────────────────────────────────────────────────

#[derive(Serialize)]
pub struct ThemeResponse {
    pub theme: String,
}

#[derive(Deserialize)]
pub struct ThemeRequest {
    pub theme: String,
}

// ── Project repository mapping ───────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct RepoMappingRequest {
    pub provider: Option<String>,
    pub namespace: Option<String>,
    pub repo_name: Option<String>,
}

pub async fn update_project_repo_mapping(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    Json(req): Json<RepoMappingRequest>,
) -> Result<Json<Project>> {
    let db = state.db.clone();
    let project = spawn_blocking(move || -> anyhow::Result<Project> {
        let conn = db.lock().unwrap();
        db::update_project_repo_mapping(
            &conn,
            &ident,
            req.provider.as_deref(),
            req.namespace.as_deref(),
            req.repo_name.as_deref(),
        )?
        .ok_or_else(|| anyhow::anyhow!("project '{ident}' not found"))
    })
    .await??;
    Ok(Json(project))
}

/// POST /v1/projects/:ident/archive
pub async fn archive_project(
    State(state): State<AppState>,
    Path(ident): Path<String>,
) -> Result<Json<Project>> {
    set_project_archived(state, ident, true).await
}

/// POST /v1/projects/:ident/restore
pub async fn restore_project(
    State(state): State<AppState>,
    Path(ident): Path<String>,
) -> Result<Json<Project>> {
    set_project_archived(state, ident, false).await
}

async fn set_project_archived(
    state: AppState,
    ident: String,
    archived: bool,
) -> Result<Json<Project>> {
    let db = state.db.clone();
    let project = spawn_blocking(move || -> anyhow::Result<Option<Project>> {
        let conn = db.lock().unwrap();
        db::set_project_archived(&conn, &ident, archived)
    })
    .await??
    .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "project not found".into()))?;
    Ok(Json(project))
}

// ── Message resolution (human inbox) ─────────────────────────────────────────

#[derive(Serialize)]
pub struct ResolveResponse {
    pub changed: bool,
}

#[derive(Serialize)]
pub struct BulkResolveResponse {
    pub resolved: usize,
}

#[derive(Deserialize, Default)]
pub struct BulkResolveRequest {
    /// Resolve root messages sent before this epoch-ms timestamp (default now).
    pub before_ms: Option<i64>,
    /// Restrict to one author kind: human | bot | webhook | agent | system.
    pub author_kind: Option<String>,
    /// Restrict to several author kinds at once.
    pub author_kinds: Option<Vec<String>>,
}

fn resolver_identity(headers: &HeaderMap) -> String {
    if let Some(user) = crate::ui_auth::request_user(headers) {
        return format!("user:{}", user.username);
    }
    match headers.get("x-agent-id").and_then(|v| v.to_str().ok()) {
        Some(agent) if !agent.is_empty() => format!("agent:{agent}"),
        _ => "user".to_string(),
    }
}

/// POST /v1/projects/:ident/messages/:id/resolve
pub async fn resolve_message(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((ident, id)): Path<(String, i64)>,
) -> Result<Json<ResolveResponse>> {
    let by = resolver_identity(&headers);
    let db = state.db.clone();
    let changed = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::resolve_message(&conn, &ident, id, &by)
    })
    .await??;
    Ok(Json(ResolveResponse { changed }))
}

/// POST /v1/projects/:ident/messages/:id/reopen
pub async fn reopen_message(
    State(state): State<AppState>,
    Path((ident, id)): Path<(String, i64)>,
) -> Result<Json<ResolveResponse>> {
    let db = state.db.clone();
    let changed = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::reopen_message(&conn, &ident, id)
    })
    .await??;
    Ok(Json(ResolveResponse { changed }))
}

/// POST /v1/projects/:ident/messages/resolve — bulk resolve open root messages.
pub async fn resolve_messages_bulk(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    body: Option<Json<BulkResolveRequest>>,
) -> Result<Json<BulkResolveResponse>> {
    let req = body.map(|Json(b)| b).unwrap_or_default();
    let by = resolver_identity(&headers);
    let before = req.before_ms.unwrap_or_else(now_ms);
    let mut kinds: Vec<String> = req
        .author_kinds
        .unwrap_or_default()
        .into_iter()
        .map(|k| k.trim().to_lowercase())
        .filter(|k| !k.is_empty())
        .collect();
    if let Some(k) = req.author_kind.map(|k| k.trim().to_lowercase()) {
        if !k.is_empty() {
            kinds.push(k);
        }
    }
    let db = state.db.clone();
    let resolved = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let kind_refs: Vec<&str> = kinds.iter().map(String::as_str).collect();
        db::resolve_messages_before(&conn, &ident, before, &kind_refs, &by)
    })
    .await??;
    Ok(Json(BulkResolveResponse { resolved }))
}

// ── Inbox API (threads, human composer) ──────────────────────────────────────

#[derive(Deserialize, Default)]
pub struct ListThreadsQuery {
    /// open | acknowledged | answered | resolved | unresolved (default) | all
    pub state: Option<String>,
    /// Comma-separated author kinds: human,agent,bot,webhook,system
    pub kind: Option<String>,
    pub agent_id: Option<String>,
    pub limit: Option<usize>,
    pub offset: Option<usize>,
}

#[derive(Serialize)]
pub struct ListThreadsResponse {
    pub threads: Vec<db::ThreadSummary>,
    pub counts: db::InboxCounts,
}

/// GET /v1/projects/:ident/messages — thread list with derived state.
pub async fn list_message_threads(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    Query(q): Query<ListThreadsQuery>,
) -> Result<Json<ListThreadsResponse>> {
    let db = state.db.clone();
    let kinds: Vec<String> = q
        .kind
        .as_deref()
        .unwrap_or("")
        .split(',')
        .map(|k| k.trim().to_lowercase())
        .filter(|k| !k.is_empty())
        .collect();
    let response = spawn_blocking(move || -> anyhow::Result<ListThreadsResponse> {
        let conn = db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            anyhow::bail!("project '{ident}' not found");
        }
        let kind_refs: Vec<&str> = kinds.iter().map(String::as_str).collect();
        let threads = db::list_message_threads(
            &conn,
            &ident,
            &db::ThreadFilter {
                state: q.state.as_deref(),
                kinds: &kind_refs,
                agent_id: q.agent_id.as_deref(),
                limit: q.limit.unwrap_or(50).min(500),
                offset: q.offset.unwrap_or(0),
            },
        )?;
        Ok(ListThreadsResponse {
            threads,
            counts: db::inbox_counts(&conn, &ident)?,
        })
    })
    .await?
    .map_err(|e| AppError(StatusCode::NOT_FOUND, format!("{e:#}")))?;
    Ok(Json(response))
}

/// GET /v1/projects/:ident/messages/:id/thread
pub async fn get_message_thread(
    State(state): State<AppState>,
    Path((ident, id)): Path<(String, i64)>,
) -> Result<Json<db::ThreadView>> {
    let db = state.db.clone();
    let thread = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_thread(&conn, &ident, id)
    })
    .await??
    .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "message not found".into()))?;
    Ok(Json(thread))
}

#[derive(Deserialize)]
pub struct HumanMessageRequest {
    pub content: String,
    /// Thread root to reply to; omitted for a new root message.
    pub parent_message_id: Option<Value>,
    /// Display name for the channel post. Defaults to "user".
    pub author: Option<String>,
    /// Resolve the thread after posting (form checkbox sends "true"/"on").
    pub resolve: Option<Value>,
}

#[derive(Serialize)]
pub struct HumanMessageResponse {
    pub message_id: i64,
    pub parent_message_id: Option<i64>,
    pub delivered: bool,
    pub delivery_error: Option<String>,
}

fn value_as_i64(v: &Value) -> Option<i64> {
    match v {
        Value::Number(n) => n.as_i64(),
        Value::String(s) => s.trim().parse().ok(),
        _ => None,
    }
}

fn value_as_bool(v: &Value) -> bool {
    match v {
        Value::Bool(b) => *b,
        Value::String(s) => matches!(
            s.trim().to_lowercase().as_str(),
            "true" | "on" | "1" | "yes"
        ),
        Value::Number(n) => n.as_i64().unwrap_or(0) != 0,
        _ => false,
    }
}

/// POST /v1/projects/:ident/messages/human — a human posts from the browser.
///
/// The row is stored first (source `user`, author kind `human`) so agents pick
/// it up on their next poll even if channel delivery fails; delivery to the
/// project room is attempted afterwards and reported in the response.
pub async fn post_human_message(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    Json(req): Json<HumanMessageRequest>,
) -> Result<Json<HumanMessageResponse>> {
    let session_user = crate::ui_auth::request_user(&headers);
    let content = req.content.trim().to_string();
    if content.is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "content must not be empty".into(),
        ));
    }
    let parent_id = req
        .parent_message_id
        .as_ref()
        .and_then(value_as_i64)
        .filter(|id| *id > 0);
    let resolve_after = req.resolve.as_ref().map(value_as_bool).unwrap_or(false);
    let author = req
        .author
        .map(|a| a.trim().to_string())
        .filter(|a| !a.is_empty())
        .or_else(|| session_user.as_ref().map(|u| u.display_name.clone()))
        .unwrap_or_else(|| "user".to_string());
    let resolved_by = session_user
        .as_ref()
        .map(|u| format!("user:{}", u.username))
        .unwrap_or_else(|| "user".to_string());

    // Store first.
    let db = state.db.clone();
    let ident_for_db = ident.clone();
    let content_for_db = content.clone();
    let (project, parent, message_id) = spawn_blocking(move || -> anyhow::Result<_> {
        let conn = db.lock().unwrap();
        let project = db::get_project(&conn, &ident_for_db)?
            .ok_or_else(|| anyhow::anyhow!("project '{ident_for_db}' not found"))?;
        let parent = match parent_id {
            Some(pid) => {
                let p = db::get_message_by_id(&conn, &project.ident, pid)?
                    .ok_or_else(|| anyhow::anyhow!("parent message {pid} not found"))?;
                // Always attach to the thread root.
                match p.parent_message_id {
                    Some(root) => db::get_message_by_id(&conn, &project.ident, root)?,
                    None => Some(p),
                }
            }
            None => None,
        };
        let message_id = db::insert_message(
            &conn,
            &Message {
                id: 0,
                project_ident: project.ident.clone(),
                source: "user".into(),
                external_message_id: None,
                content: content_for_db,
                sent_at: now_ms(),
                confirmed_at: None,
                parent_message_id: parent.as_ref().map(|p| p.id),
                agent_id: None,
                message_type: if parent.is_some() {
                    "reply".into()
                } else {
                    "message".into()
                },
                subject: None,
                hostname: None,
                event_at: None,
                deliver_to_agents: true,
                author_kind: Some("human".into()),
                resolved_at: None,
                resolved_by: None,
            },
        )?;
        if resolve_after {
            if let Some(root) = parent.as_ref() {
                db::resolve_message(&conn, &project.ident, root.id, &resolved_by)?;
            }
        }
        Ok((project, parent, message_id))
    })
    .await?
    .map_err(|e| AppError(StatusCode::NOT_FOUND, format!("{e:#}")))?;

    // Then deliver to the channel, best effort.
    let text = format!("**{author}** (via gateway): {content}");
    let delivery: std::result::Result<String, String> =
        match state.plugins.get(&project.channel_name) {
            None => Err(format!(
                "channel plugin '{}' is not configured",
                project.channel_name
            )),
            Some(plugin) => {
                let plugin = plugin.clone();
                match ensure_project_room(
                    &state,
                    &project.ident,
                    plugin.as_ref(),
                    project.room_id.clone(),
                )
                .await
                {
                    Err(e) => Err(e.1),
                    Ok(room_id) => {
                        let result =
                            match parent.as_ref().and_then(|p| p.external_message_id.clone()) {
                                Some(ext) => plugin.reply(&room_id, &ext, &text).await,
                                None => plugin.send(&room_id, &text).await,
                            };
                        result.map_err(|e| format!("{e:#}"))
                    }
                }
            }
        };
    let (delivered, delivery_error) = match delivery {
        Ok(external_id) => {
            let db = state.db.clone();
            spawn_blocking(move || {
                let conn = db.lock().unwrap();
                conn.execute(
                    "UPDATE messages SET external_message_id = ?1 WHERE id = ?2",
                    rusqlite::params![external_id, message_id],
                )
            })
            .await??;
            (true, None)
        }
        Err(e) => {
            tracing::warn!("human message {message_id} stored but not delivered: {e}");
            (false, Some(e))
        }
    };

    Ok(Json(HumanMessageResponse {
        message_id,
        parent_message_id: parent.map(|p| p.id),
        delivered,
        delivery_error,
    }))
}

// ── Project links + rooms ────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct ProjectLinkRequest {
    pub label: String,
    pub url: String,
}

/// GET /v1/projects/:ident/links
pub async fn list_project_links(
    State(state): State<AppState>,
    Path(ident): Path<String>,
) -> Result<Json<Vec<db::ProjectLink>>> {
    let db = state.db.clone();
    let links = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_project_links(&conn, &ident)
    })
    .await??;
    Ok(Json(links))
}

/// POST /v1/projects/:ident/links
pub async fn add_project_link(
    State(state): State<AppState>,
    Path(ident): Path<String>,
    Json(req): Json<ProjectLinkRequest>,
) -> Result<Json<db::ProjectLink>> {
    let db = state.db.clone();
    let link = spawn_blocking(move || -> anyhow::Result<db::ProjectLink> {
        let conn = db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            anyhow::bail!("project '{ident}' not found");
        }
        db::add_project_link(&conn, &ident, &req.label, &req.url)
    })
    .await?
    .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?;
    Ok(Json(link))
}

/// DELETE /v1/projects/:ident/links/:id
pub async fn delete_project_link(
    State(state): State<AppState>,
    Path((ident, id)): Path<(String, i64)>,
) -> Result<Json<ResolveResponse>> {
    let db = state.db.clone();
    let changed = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::delete_project_link(&conn, &ident, id)
    })
    .await??;
    Ok(Json(ResolveResponse { changed }))
}

/// POST /v1/projects/:ident/room — create the channel room for an adhoc
/// project ahead of its first message.
pub async fn create_project_room(
    State(state): State<AppState>,
    Path(ident): Path<String>,
) -> Result<Json<Project>> {
    let (channel_name, room_id) = {
        let conn = state.db.lock().unwrap();
        match db::get_project(&conn, &ident)? {
            Some(p) => (p.channel_name, p.room_id),
            None => {
                return Err(AppError(
                    StatusCode::NOT_FOUND,
                    format!("project '{ident}' not found"),
                ))
            }
        }
    };
    let plugin = state
        .plugins
        .get(&channel_name)
        .ok_or_else(|| {
            AppError(
                StatusCode::SERVICE_UNAVAILABLE,
                format!("channel plugin '{channel_name}' is not configured"),
            )
        })?
        .clone();
    ensure_project_room(&state, &ident, plugin.as_ref(), room_id).await?;
    let db = state.db.clone();
    let project = spawn_blocking(move || -> anyhow::Result<Option<Project>> {
        let conn = db.lock().unwrap();
        db::get_project(&conn, &ident)
    })
    .await??
    .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "project not found".into()))?;
    Ok(Json(project))
}

// ── Agent memory gateway ─────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct ListMemoriesQuery {
    pub q: Option<String>,
    #[serde(rename = "type", alias = "memory_type")]
    pub memory_type: Option<String>,
    pub tag: Option<String>,
    pub since_revision: Option<i64>,
    pub limit: Option<usize>,
    #[serde(default)]
    pub include_tombstones: bool,
}

#[derive(Deserialize)]
pub struct PullMemoriesRequest {
    pub since_revision: Option<i64>,
    #[serde(default)]
    pub known: Vec<db::MemoryKnownRevision>,
    pub page_size: Option<usize>,
    #[serde(default = "default_true")]
    pub include_tombstones: bool,
}

#[derive(Deserialize)]
pub struct PushMemoriesRequest {
    #[serde(default)]
    pub memories: Vec<db::MemoryPushItem>,
}

fn default_true() -> bool {
    true
}

fn optional_header(headers: &HeaderMap, name: &str) -> Option<String> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_string)
}

pub async fn memory_capabilities_handler() -> Json<serde_json::Value> {
    Json(crate::memory_okf::capabilities())
}

fn memory_okf_requested(headers: &HeaderMap) -> bool {
    optional_header(headers, "x-memory-capabilities").is_some_and(|value| {
        value
            .split(',')
            .any(|token| token.trim() == crate::memory_okf::CAPABILITY)
    })
}

fn project_memory_envelopes(memories: &mut [db::MemoryRecord], headers: &HeaderMap) {
    if !memory_okf_requested(headers) {
        for memory in memories {
            memory.concept_hash = None;
            memory.okf = None;
        }
    }
}

fn memory_push_source(headers: &HeaderMap) -> db::MemoryPushSource {
    db::MemoryPushSource {
        agent_id: optional_header(headers, "x-agent-id"),
        hostname: optional_header(headers, "x-agent-hostname"),
        client_id: optional_header(headers, "x-memory-client-id")
            .or_else(|| optional_header(headers, "x-agent-id")),
        client_version: optional_header(headers, "x-memory-client-version"),
    }
}

pub async fn list_memories_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    Query(q): Query<ListMemoriesQuery>,
) -> Result<Json<db::MemorySearchResponse>> {
    let db = state.db.clone();
    let (project_exists, response) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        // The reserved global-memory route is always addressable, even before
        // any global memory exists (it returns an empty response, not a 404).
        if !db::is_global_memory_ident(&ident) && db::get_project(&conn, &ident)?.is_none() {
            return Ok::<_, anyhow::Error>((false, None));
        }
        let filters = db::MemoryFilters {
            query: q.q.as_deref(),
            memory_type: q.memory_type.as_deref(),
            tag: q.tag.as_deref(),
            since_revision: q.since_revision,
            include_tombstones: q.include_tombstones,
            limit: q.limit,
        };
        Ok((
            true,
            Some(db::search_project_memories(&conn, &ident, &filters)?),
        ))
    })
    .await??;
    if !project_exists {
        return Err(AppError(
            StatusCode::NOT_FOUND,
            "project not found".to_string(),
        ));
    }
    let mut response = response.expect("memory search response exists for project");
    project_memory_envelopes(&mut response.memories, &headers);
    Ok(Json(response))
}

pub async fn pull_memories_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    Json(req): Json<PullMemoriesRequest>,
) -> Result<Json<db::MemoryPullResponse>> {
    let db = state.db.clone();
    let (project_exists, response) = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        // `__global__` is always pullable; an empty global store yields an empty
        // response (server_revision 0) rather than a 404.
        if !db::is_global_memory_ident(&ident) && db::get_project(&conn, &ident)?.is_none() {
            return Ok::<_, anyhow::Error>((false, None));
        }
        Ok((
            true,
            Some(db::pull_project_memories(
                &conn,
                &ident,
                req.since_revision,
                &req.known,
                req.include_tombstones,
                req.page_size,
            )?),
        ))
    })
    .await??;
    if !project_exists {
        return Err(AppError(
            StatusCode::NOT_FOUND,
            "project not found".to_string(),
        ));
    }
    let mut response = response.expect("memory pull response exists for project");
    project_memory_envelopes(&mut response.memories, &headers);
    Ok(Json(response))
}

pub async fn push_memories_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    Json(req): Json<PushMemoriesRequest>,
) -> Result<Json<db::MemoryPushResponse>> {
    if req.memories.len() > db::MEMORY_BATCH_LIMIT {
        return Err(AppError(
            StatusCode::PAYLOAD_TOO_LARGE,
            format!(
                "memory push batch exceeds {} records",
                db::MEMORY_BATCH_LIMIT
            ),
        ));
    }
    let source = memory_push_source(&headers);
    let db = state.db.clone();
    let response = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        // Unknown normal projects return a structured push response rather
        // than a transport 404. All-scope clients push one project at a time;
        // a transport error would abort the remaining projects.
        if !db::is_global_memory_ident(&ident) && db::get_project(&conn, &ident)?.is_none() {
            return Ok::<_, anyhow::Error>(db::reject_missing_project_memories(
                &ident,
                &source,
                &req.memories,
            ));
        }
        // Global pushes skip the existence check: `push_project_memories`
        // materializes the reserved `__global__` project row on demand so the
        // memory foreign key holds.
        db::push_project_memories(&conn, &ident, &source, &req.memories)
    })
    .await??;
    Ok(Json(response))
}

/// `GET /v1/memories/projects` — discovery endpoint for `memory pull --all`.
///
/// Returns every project ident the gateway holds durable memories for (the
/// gateway is the source of truth, so this includes projects that do not exist
/// on the caller's machine), plus `__global__` when global memories exist.
pub async fn discover_memory_projects_handler(
    State(state): State<AppState>,
) -> Result<Json<db::MemoryProjectsResponse>> {
    let db = state.db.clone();
    let response = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::discover_memory_projects(&conn)
    })
    .await??;
    Ok(Json(response))
}

// ── POST /v1/projects ─────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct RegisterProjectRequest {
    /// Raw project identity (git remote URL or directory name).
    pub ident: String,
    /// Which channel plugin to use. Defaults to gateway's DEFAULT_CHANNEL.
    pub channel: Option<String>,
    /// Git remote (or agent-tools canonical ident such as
    /// `github.com/nitecon/agent-gateway.git`). When present the repository
    /// mapping is derived from it and the project is registered as a `repo`
    /// project; otherwise it is an `adhoc` project with no channel room until
    /// the first message is sent.
    pub repo_url: Option<String>,
}

#[derive(Serialize)]
pub struct RegisterProjectResponse {
    pub ident: String,
    pub channel_name: String,
    pub room_id: String,
    pub kind: String,
    pub repo_full_name: Option<String>,
}

pub async fn register_project(
    State(state): State<AppState>,
    Json(body): Json<RegisterProjectRequest>,
) -> Result<Json<RegisterProjectResponse>> {
    let project_ident = sanitize_ident(&body.ident);
    let channel_name = body
        .channel
        .unwrap_or_else(|| state.default_channel.clone());
    // Prefer the explicit remote; fall back to the ident itself when a client
    // still sends the full remote URL as the identity.
    let repo = body
        .repo_url
        .as_deref()
        .and_then(crate::projects::parse_remote)
        .or_else(|| crate::projects::parse_remote(&body.ident));

    // Existing project: idempotent return, but heal an empty mapping when the
    // client now tells us the remote.
    {
        let db = state.db.clone();
        let ident = project_ident.clone();
        let repo_for_backfill = repo.clone();
        let existing = spawn_blocking(move || -> anyhow::Result<Option<Project>> {
            let conn = db.lock().unwrap();
            if let Some(repo) = repo_for_backfill.as_ref() {
                db::backfill_project_repo(&conn, &ident, repo)?;
            }
            db::get_project(&conn, &ident)
        })
        .await??;
        if let Some(existing) = existing {
            return Ok(Json(RegisterProjectResponse {
                ident: existing.ident,
                channel_name: existing.channel_name,
                room_id: existing.room_id,
                kind: existing.kind,
                repo_full_name: existing.repo_full_name,
            }));
        }
    }

    // Repo projects get their room now; adhoc projects (bare directory names)
    // get one lazily on first send so stray `cd`s do not create channels.
    let (kind, room_id) = match repo.as_ref() {
        Some(_) => {
            let plugin = state
                .plugins
                .get(&channel_name)
                .ok_or_else(|| {
                    AppError(
                        StatusCode::BAD_REQUEST,
                        format!("unknown channel plugin: '{channel_name}'"),
                    )
                })?
                .clone();
            (
                db::PROJECT_KIND_REPO.to_string(),
                plugin.ensure_room(&project_ident).await?,
            )
        }
        None => (db::PROJECT_KIND_ADHOC.to_string(), String::new()),
    };

    let project = Project {
        ident: project_ident.clone(),
        channel_name: channel_name.clone(),
        room_id: room_id.clone(),
        last_msg_id: None,
        created_at: now_ms(),
        repo_provider: repo.as_ref().map(|r| r.provider.clone()),
        repo_namespace: repo.as_ref().map(|r| r.namespace.clone()),
        repo_name: repo.as_ref().map(|r| r.name.clone()),
        repo_full_name: repo.as_ref().map(|r| r.full_name()),
        kind: kind.clone(),
        archived_at: None,
        canonical_remote: repo.as_ref().map(|r| r.canonical.clone()),
    };

    let db = state.db.clone();
    let project_clone = project.clone();
    let stored = spawn_blocking(move || -> anyhow::Result<Option<Project>> {
        let conn = db.lock().unwrap();
        db::insert_project(&conn, &project_clone)?;
        db::get_project(&conn, &project_clone.ident)
    })
    .await??
    .unwrap_or(project);

    Ok(Json(RegisterProjectResponse {
        ident: stored.ident,
        channel_name: stored.channel_name,
        room_id: stored.room_id,
        kind: stored.kind,
        repo_full_name: stored.repo_full_name,
    }))
}

/// Resolve the plugin room for an outbound send, creating it on first use for
/// adhoc projects that were registered without one.
pub(crate) async fn ensure_project_room(
    state: &AppState,
    ident: &str,
    plugin: &dyn crate::channel::ChannelPlugin,
    room_id: String,
) -> Result<String> {
    if !room_id.is_empty() {
        return Ok(room_id);
    }
    let created = plugin.ensure_room(ident).await?;
    plugin.register_room(&created, None);
    let db = state.db.clone();
    let ident = ident.to_string();
    let persisted = created.clone();
    spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::set_project_room(&conn, &ident, &persisted)
    })
    .await??;
    Ok(created)
}

// ── POST /v1/projects/:ident/messages ─────────────────────────────────────────

#[derive(Deserialize)]
pub struct SendMessageRequest {
    /// Back-compat alias for `body`. If both are set, `body` wins.
    pub content: Option<String>,
    pub body: Option<String>,
    pub subject: Option<String>,
    pub hostname: Option<String>,
    /// Event time in epoch milliseconds. Defaults to now() when omitted.
    pub event_at: Option<i64>,
}

#[derive(Serialize)]
pub struct SendMessageResponse {
    pub message_id: i64,
    pub external_message_id: String,
}

pub async fn send_message(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    Json(req): Json<SendMessageRequest>,
) -> Result<Json<SendMessageResponse>> {
    let (channel_name, room_id) = {
        let conn = state.db.lock().unwrap();
        match db::get_project(&conn, &ident)? {
            Some(p) => (p.channel_name, p.room_id),
            None => {
                return Err(AppError(
                    StatusCode::NOT_FOUND,
                    format!("project '{}' not found", ident),
                ))
            }
        }
    };

    let plugin = state
        .plugins
        .get(&channel_name)
        .ok_or_else(|| {
            AppError(
                StatusCode::SERVICE_UNAVAILABLE,
                format!("channel plugin '{channel_name}' is not configured"),
            )
        })?
        .clone();
    let room_id = ensure_project_room(&state, &ident, plugin.as_ref(), room_id).await?;

    let agent_id = extract_agent_id(&headers);
    let body_text = req.body.or(req.content).unwrap_or_default();
    if body_text.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "request must include non-empty 'body' (or 'content')".into(),
        ));
    }
    let outbound = build_outbound(
        &agent_id,
        body_text,
        req.subject,
        req.hostname,
        req.event_at,
    );
    let external_id = plugin.send_structured(&room_id, &outbound).await?;

    let msg = Message {
        id: 0,
        project_ident: ident.clone(),
        source: "agent".into(),
        external_message_id: Some(external_id.clone()),
        content: outbound.body.clone(),
        sent_at: now_ms(),
        confirmed_at: None,
        parent_message_id: None,
        agent_id: Some(agent_id.clone()),
        message_type: "message".into(),
        subject: Some(outbound.subject.clone()),
        hostname: Some(outbound.hostname.clone()),
        event_at: Some(outbound.event_at),
        deliver_to_agents: false,
        author_kind: Some("agent".to_string()),
        resolved_at: None,
        resolved_by: None,
    };

    let db = state.db.clone();
    let ident_clone = ident.clone();
    let aid = agent_id;
    let row_id = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::upsert_agent(&conn, &ident_clone, &aid)?;
        db::insert_message(&conn, &msg)
    })
    .await??;

    Ok(Json(SendMessageResponse {
        message_id: row_id,
        external_message_id: external_id,
    }))
}

// ── Skills API ────────────────────────────────────────────────────────────────

#[derive(serde::Serialize)]
pub struct SkillUploadResponse {
    pub name: String,
    pub kind: String,
    pub size: i64,
    pub checksum: String,
}

pub async fn upload_skill(
    State(state): State<AppState>,
    Path(name): Path<String>,
    headers: HeaderMap,
    body: Bytes,
) -> Result<Json<SkillUploadResponse>> {
    use sha2::{Digest, Sha256};

    let content_type = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("application/zip");

    // X-Kind header takes precedence; fall back to Content-Type detection.
    let kind = match headers
        .get("x-kind")
        .and_then(|v| v.to_str().ok())
        .map(|s| s.to_lowercase())
        .as_deref()
    {
        Some("skill") => "skill".to_string(),
        Some("command") => "command".to_string(),
        Some("agent") => "agent".to_string(),
        Some(other) => {
            return Err(AppError(
                StatusCode::BAD_REQUEST,
                format!("invalid X-Kind: '{other}'"),
            ))
        }
        None => {
            if content_type.starts_with("text/markdown") {
                "command".to_string()
            } else {
                "skill".to_string()
            }
        }
    };

    let is_text = kind == "command" || kind == "agent";

    let (zip_data, content, size) = if is_text {
        let text = String::from_utf8(body.to_vec())
            .map_err(|_| AppError(StatusCode::BAD_REQUEST, "body is not valid UTF-8".into()))?;
        if text.is_empty() {
            return Err(AppError(StatusCode::BAD_REQUEST, "body is empty".into()));
        }
        let size = text.len() as i64;
        (vec![], Some(text), size)
    } else {
        let zip = body.to_vec();
        let size = zip.len() as i64;
        (zip, None, size)
    };

    let mut hasher = Sha256::new();
    match &content {
        Some(text) => hasher.update(text.as_bytes()),
        None => hasher.update(&zip_data),
    }
    let checksum = hex::encode(hasher.finalize());

    let record = db::SkillRecord {
        name: name.clone(),
        kind: kind.clone(),
        zip_data,
        content,
        size,
        checksum: checksum.clone(),
        uploaded_at: db::now_ms(),
    };
    let db = state.db.clone();
    spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::upsert_skill(&conn, &record)
    })
    .await??;

    Ok(Json(SkillUploadResponse {
        name,
        kind,
        size,
        checksum,
    }))
}

/// Multipart variant of the skill-upload endpoint. Accepts a form with:
///   - `kind` (text): `"skill" | "command" | "agent"` — required
///   - `content` (text): markdown body — required when kind is `command|agent`
///   - `file` (binary): zip bytes — required when kind is `skill`
///
/// Designed for ndesign's `data-nd-action` form serializer, which posts
/// `multipart/form-data` when any field is a file. Persists into the same
/// `skills` table as the raw-body PUT variant.
pub async fn upload_skill_multipart(
    State(state): State<AppState>,
    Path(name): Path<String>,
    mut multipart: axum::extract::Multipart,
) -> Result<Json<SkillUploadResponse>> {
    use sha2::{Digest, Sha256};

    let mut kind_field: Option<String> = None;
    let mut content_field: Option<String> = None;
    let mut file_bytes: Option<Vec<u8>> = None;

    while let Some(field) = multipart.next_field().await.map_err(|e| {
        AppError(
            StatusCode::BAD_REQUEST,
            format!("failed to parse multipart body: {e}"),
        )
    })? {
        let field_name = field.name().unwrap_or("").to_string();
        match field_name.as_str() {
            "kind" => {
                let text = field.text().await.map_err(|e| {
                    AppError(
                        StatusCode::BAD_REQUEST,
                        format!("failed to read 'kind' field: {e}"),
                    )
                })?;
                kind_field = Some(text.trim().to_lowercase());
            }
            "content" => {
                let text = field.text().await.map_err(|e| {
                    AppError(
                        StatusCode::BAD_REQUEST,
                        format!("failed to read 'content' field: {e}"),
                    )
                })?;
                content_field = Some(text);
            }
            "file" => {
                let bytes = field.bytes().await.map_err(|e| {
                    AppError(
                        StatusCode::BAD_REQUEST,
                        format!("failed to read 'file' field: {e}"),
                    )
                })?;
                file_bytes = Some(bytes.to_vec());
            }
            _ => {
                // Silently ignore unknown fields — ndesign's serializer may
                // add incidental metadata fields that are not part of the
                // upload contract.
            }
        }
    }

    let kind = match kind_field.as_deref() {
        Some("skill") => "skill".to_string(),
        Some("command") => "command".to_string(),
        Some("agent") => "agent".to_string(),
        Some(other) => {
            return Err(AppError(
                StatusCode::BAD_REQUEST,
                format!("invalid 'kind': '{other}' (must be skill|command|agent)"),
            ))
        }
        None => {
            return Err(AppError(
                StatusCode::BAD_REQUEST,
                "'kind' field is required".into(),
            ))
        }
    };

    let (zip_data, content, size) = match kind.as_str() {
        "skill" => {
            let bytes = file_bytes.ok_or_else(|| {
                AppError(
                    StatusCode::BAD_REQUEST,
                    "'file' field is required when kind is 'skill'".into(),
                )
            })?;
            if bytes.is_empty() {
                return Err(AppError(
                    StatusCode::BAD_REQUEST,
                    "'file' must be non-empty".into(),
                ));
            }
            let size = bytes.len() as i64;
            (bytes, None, size)
        }
        _ => {
            // command | agent
            let text = content_field.ok_or_else(|| {
                AppError(
                    StatusCode::BAD_REQUEST,
                    format!("'content' field is required when kind is '{kind}'"),
                )
            })?;
            if text.trim().is_empty() {
                return Err(AppError(
                    StatusCode::BAD_REQUEST,
                    "'content' must be non-empty".into(),
                ));
            }
            let size = text.len() as i64;
            (vec![], Some(text), size)
        }
    };

    let mut hasher = Sha256::new();
    match &content {
        Some(text) => hasher.update(text.as_bytes()),
        None => hasher.update(&zip_data),
    }
    let checksum = hex::encode(hasher.finalize());

    let record = db::SkillRecord {
        name: name.clone(),
        kind: kind.clone(),
        zip_data,
        content,
        size,
        checksum: checksum.clone(),
        uploaded_at: db::now_ms(),
    };
    let db = state.db.clone();
    spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::upsert_skill(&conn, &record)
    })
    .await??;

    Ok(Json(SkillUploadResponse {
        name,
        kind,
        size,
        checksum,
    }))
}

// ── POST /v1/skills/:name (JSON upsert for command/agent) ────────────────────

/// Request body for the JSON upsert endpoint.
///
/// `kind` must be `"command"` or `"agent"` — zip-backed skills cannot be
/// upserted via this endpoint and must use the existing `PUT` (raw body) or
/// `POST .../multipart` variants, which accept binary data.
///
/// `content` is the markdown body and must be non-empty after trimming.
#[derive(Deserialize)]
pub struct JsonSkillRequest {
    pub kind: String,
    pub content: String,
}

#[derive(Deserialize)]
pub struct NamedJsonSkillRequest {
    pub name: String,
    pub kind: String,
    pub content: String,
}

async fn persist_text_skill(
    state: AppState,
    name: String,
    kind: String,
    content: String,
) -> Result<Json<SkillUploadResponse>> {
    use sha2::{Digest, Sha256};

    if name.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "'name' must be non-empty".into(),
        ));
    }

    let kind = match kind.as_str() {
        "command" => "command".to_string(),
        "agent" => "agent".to_string(),
        other => {
            return Err(AppError(
                StatusCode::BAD_REQUEST,
                format!(
                    "invalid 'kind': '{other}' (must be 'command' or 'agent'; \
                     zip skills use PUT /v1/skills/:name)"
                ),
            ))
        }
    };

    if content.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "'content' must be non-empty".into(),
        ));
    }

    let name = name.trim().to_string();
    let size = content.len() as i64;
    let mut hasher = Sha256::new();
    hasher.update(content.as_bytes());
    let checksum = hex::encode(hasher.finalize());

    let record = db::SkillRecord {
        name: name.clone(),
        kind: kind.clone(),
        zip_data: vec![],
        content: Some(content),
        size,
        checksum: checksum.clone(),
        uploaded_at: db::now_ms(),
    };
    let db = state.db.clone();
    spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::upsert_skill(&conn, &record)
    })
    .await??;

    Ok(Json(SkillUploadResponse {
        name,
        kind,
        size,
        checksum,
    }))
}

/// Create-or-update a text-kind skill (`command` or `agent`) via JSON.
///
/// ndesign's `data-nd-action` on a `<form>` serializes named inputs into a
/// JSON body (not multipart). This endpoint is the JSON-native create/edit
/// path used by the Commands and Agents control-panel pages. Zip skills are
/// not supported here — they are managed exclusively from the agent-tools
/// CLI via the raw-body `PUT` endpoint.
///
/// Validation:
/// * `name` — must be non-empty (400).
/// * `kind` — must be exactly `"command"` or `"agent"` (400 on anything else,
///   including `"skill"`).
/// * `content` — must be non-empty after `trim()` (400).
///
/// On success the same `SkillUploadResponse` shape as the raw-body PUT is
/// returned so clients can treat the two endpoints uniformly.
pub async fn upsert_skill_json(
    State(state): State<AppState>,
    Path(name): Path<String>,
    Json(req): Json<JsonSkillRequest>,
) -> Result<Json<SkillUploadResponse>> {
    persist_text_skill(state, name, req.kind, req.content).await
}

pub async fn upsert_named_skill_json(
    State(state): State<AppState>,
    Json(req): Json<NamedJsonSkillRequest>,
) -> Result<Json<SkillUploadResponse>> {
    persist_text_skill(state, req.name, req.kind, req.content).await
}

#[derive(Deserialize)]
pub struct ListSkillsQuery {
    /// Optional filter — when set, restrict the response to a single kind.
    pub kind: Option<String>,
}

pub async fn list_skills_handler(
    State(state): State<AppState>,
    axum::extract::Query(q): axum::extract::Query<ListSkillsQuery>,
) -> Result<Json<Vec<db::SkillMeta>>> {
    let kind = match q.kind.as_deref() {
        None | Some("") => None,
        Some(k) => {
            if k != "skill" && k != "command" && k != "agent" {
                return Err(AppError(
                    StatusCode::BAD_REQUEST,
                    format!("invalid kind '{k}': must be skill|command|agent"),
                ));
            }
            Some(k.to_string())
        }
    };

    let db = state.db.clone();
    let skills = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_skills(&conn, kind.as_deref())
    })
    .await??;
    Ok(Json(skills))
}

pub async fn download_skill(
    State(state): State<AppState>,
    Path(name): Path<String>,
) -> Result<impl IntoResponse> {
    let db = state.db.clone();
    let record = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_skill(&conn, &name)
    })
    .await??;

    match record {
        None => Err(AppError(StatusCode::NOT_FOUND, "not found".into())),
        Some(r) if r.kind == "command" || r.kind == "agent" => {
            let text = r.content.unwrap_or_default();
            let mut headers = HeaderMap::new();
            headers.insert(
                header::CONTENT_TYPE,
                HeaderValue::from_static("text/markdown; charset=utf-8"),
            );
            if let Ok(v) = HeaderValue::from_str(&r.kind) {
                headers.insert("x-kind", v);
            }
            let cd = format!("attachment; filename=\"{}.md\"", r.name);
            headers.insert(
                header::CONTENT_DISPOSITION,
                HeaderValue::from_str(&cd).unwrap_or(HeaderValue::from_static("attachment")),
            );
            Ok((headers, text.into_bytes()))
        }
        Some(r) => {
            let mut headers = HeaderMap::new();
            headers.insert(
                header::CONTENT_TYPE,
                HeaderValue::from_static("application/zip"),
            );
            if let Ok(v) = HeaderValue::from_str(&r.kind) {
                headers.insert("x-kind", v);
            }
            let cd = format!("attachment; filename=\"{}.zip\"", r.name);
            headers.insert(
                header::CONTENT_DISPOSITION,
                HeaderValue::from_str(&cd).unwrap_or(HeaderValue::from_static("attachment")),
            );
            Ok((headers, r.zip_data))
        }
    }
}

pub async fn delete_skill_handler(
    State(state): State<AppState>,
    Path(name): Path<String>,
) -> Result<StatusCode> {
    let db = state.db.clone();
    let existed = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::delete_skill(&conn, &name)
    })
    .await??;

    if existed {
        Ok(StatusCode::NO_CONTENT)
    } else {
        Err(AppError(StatusCode::NOT_FOUND, "skill not found".into()))
    }
}

// ── GET /v1/skills/:name/content ──────────────────────────────────────────────

#[derive(Serialize)]
pub struct SkillContentResponse {
    pub name: String,
    pub kind: String,
    pub content: Option<String>,
}

pub async fn get_skill_content(
    State(state): State<AppState>,
    Path(name): Path<String>,
) -> Result<Json<SkillContentResponse>> {
    let db = state.db.clone();
    let record = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_skill(&conn, &name)
    })
    .await??;

    match record {
        None => Err(AppError(StatusCode::NOT_FOUND, "not found".into())),
        Some(r) => Ok(Json(SkillContentResponse {
            name: r.name,
            kind: r.kind,
            content: r.content,
        })),
    }
}

// ── GET /skills, /commands, /agents (control-panel list pages) ──────────────

// ── GET / (dashboard) ─────────────────────────────────────────────────────────

// ── Patterns API ─────────────────────────────────────────────────────────────

#[derive(Deserialize)]
pub struct ListPatternsQuery {
    pub q: Option<String>,
    pub label: Option<String>,
    pub version: Option<String>,
    pub state: Option<String>,
    pub superseded_by: Option<String>,
}

#[derive(Deserialize)]
pub struct CreatePatternRequest {
    pub title: String,
    pub slug: Option<String>,
    pub summary: Option<String>,
    pub body: String,
    pub labels: Option<serde_json::Value>,
    pub version: String,
    pub state: String,
    pub superseded_by: Option<String>,
    /// Defaults to X-Agent-Id header, or "user" when the header is absent.
    pub author: Option<String>,
}

#[derive(Deserialize)]
pub struct UpdatePatternRequest {
    pub title: Option<String>,
    pub slug: Option<String>,
    /// `Some(null)` clears the summary; absent leaves it untouched.
    pub summary: Option<serde_json::Value>,
    pub body: Option<String>,
    pub labels: Option<serde_json::Value>,
    pub version: Option<String>,
    pub state: Option<String>,
    pub superseded_by: Option<serde_json::Value>,
}

fn validate_pattern_version_field(version: &str) -> Result<()> {
    if version == "draft" || version == "latest" || version == "superseded" {
        Ok(())
    } else {
        Err(AppError(
            StatusCode::BAD_REQUEST,
            format!("invalid version '{version}': must be draft|latest|superseded"),
        ))
    }
}

fn validate_pattern_state_field(state: &str) -> Result<()> {
    if state == "active" || state == "archived" {
        Ok(())
    } else {
        Err(AppError(
            StatusCode::BAD_REQUEST,
            format!("invalid state '{state}': must be active|archived"),
        ))
    }
}

fn decode_labels_field(field: &str, value: Option<serde_json::Value>) -> Result<Vec<String>> {
    match value {
        None | Some(serde_json::Value::Null) => Ok(Vec::new()),
        Some(serde_json::Value::Array(items)) => items
            .into_iter()
            .map(|v| match v {
                serde_json::Value::String(s) => Ok(s.trim().to_string()),
                _ => Err(AppError(
                    StatusCode::BAD_REQUEST,
                    format!("'{field}' must be an array of strings or a comma-separated string"),
                )),
            })
            .filter_map(|r| match r {
                Ok(s) if s.is_empty() => None,
                other => Some(other),
            })
            .collect(),
        Some(serde_json::Value::String(s)) => Ok(s
            .split(',')
            .map(str::trim)
            .filter(|s| !s.is_empty())
            .map(str::to_string)
            .collect()),
        Some(_) => Err(AppError(
            StatusCode::BAD_REQUEST,
            format!("'{field}' must be an array of strings or a comma-separated string"),
        )),
    }
}

fn decode_optional_labels_field(
    field: &str,
    value: Option<serde_json::Value>,
) -> Result<Option<Vec<String>>> {
    match value {
        None => Ok(None),
        Some(v) => decode_labels_field(field, Some(v)).map(Some),
    }
}

pub async fn list_patterns_handler(
    State(state): State<AppState>,
    Query(q): Query<ListPatternsQuery>,
) -> Result<Json<Vec<db::PatternSummary>>> {
    if let Some(version) = q.version.as_deref() {
        validate_pattern_version_field(version.trim())?;
    }
    if let Some(state) = q.state.as_deref() {
        validate_pattern_state_field(state.trim())?;
    }
    let db = state.db.clone();
    let query = q.q;
    let label = q.label;
    let version = q.version;
    let state_value = q.state;
    let superseded_by = q.superseded_by;
    let patterns = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let filters = db::PatternFilters {
            query: query.as_deref(),
            label: label.as_deref(),
            version: version.as_deref(),
            state: state_value.as_deref(),
            superseded_by: superseded_by.as_deref(),
        };
        db::list_patterns(&conn, &filters)
    })
    .await??;
    Ok(Json(patterns))
}

pub async fn create_pattern_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<CreatePatternRequest>,
) -> Result<Json<db::Pattern>> {
    if req.title.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "title is required".into(),
        ));
    }
    if req.body.trim().is_empty() {
        return Err(AppError(StatusCode::BAD_REQUEST, "body is required".into()));
    }
    validate_pattern_version_field(req.version.trim())?;
    validate_pattern_state_field(req.state.trim())?;
    let superseded_by = req
        .superseded_by
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty());
    if req.version.trim() == "superseded" && superseded_by.is_none() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "superseded_by is required when version is superseded".into(),
        ));
    }
    if req.version.trim() != "superseded" && superseded_by.is_some() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "superseded_by can only be set when version is superseded".into(),
        ));
    }

    let labels = decode_labels_field("labels", req.labels)?;
    let author = resolve_identity(req.author, &headers);
    let db = state.db.clone();
    let title = req.title;
    let slug = req.slug;
    let summary = req.summary;
    let body = req.body;
    let version = req.version;
    let state_value = req.state;
    let superseded_by = req.superseded_by;

    let pattern = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::insert_pattern(
            &conn,
            title.trim(),
            slug.as_deref().map(str::trim),
            summary.as_deref().map(str::trim).filter(|s| !s.is_empty()),
            &body,
            &labels,
            version.trim(),
            state_value.trim(),
            superseded_by.as_deref().map(str::trim),
            &author,
        )
    })
    .await??;
    Ok(Json(pattern))
}

pub async fn get_pattern_handler(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<Json<db::Pattern>> {
    let db = state.db.clone();
    let id_for_lookup = id.clone();
    let pattern = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::get_pattern(&conn, &id_for_lookup)
    })
    .await??;

    match pattern {
        Some(pattern) => Ok(Json(pattern)),
        None => Err(AppError(
            StatusCode::NOT_FOUND,
            format!("pattern '{}' not found", id),
        )),
    }
}

pub async fn update_pattern_handler(
    State(state): State<AppState>,
    Path(id): Path<String>,
    Json(req): Json<UpdatePatternRequest>,
) -> Result<Json<db::Pattern>> {
    let summary = decode_nullable_string("summary", req.summary)?;
    let labels = decode_optional_labels_field("labels", req.labels)?;
    let superseded_by = decode_nullable_string("superseded_by", req.superseded_by)?;
    if let Some(version) = req.version.as_deref() {
        validate_pattern_version_field(version.trim())?;
    }
    if let Some(state) = req.state.as_deref() {
        validate_pattern_state_field(state.trim())?;
    }
    let db = state.db.clone();
    let id_for_update = id.clone();
    let title = req.title;
    let slug = req.slug;
    let body = req.body;
    let version = req.version;
    let state_value = req.state;

    let pattern = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        let upd = db::PatternUpdate {
            title: title.as_deref().map(str::trim).filter(|s| !s.is_empty()),
            slug: slug.as_deref().map(str::trim).filter(|s| !s.is_empty()),
            summary: summary.as_ref().map(|inner| inner.as_deref()),
            body: body.as_deref(),
            labels: labels.as_deref(),
            version: version.as_deref().map(str::trim),
            state: state_value.as_deref().map(str::trim),
            superseded_by: superseded_by.as_ref().map(|inner| inner.as_deref()),
        };
        db::update_pattern(&conn, &id_for_update, &upd)
    })
    .await??;

    match pattern {
        Some(pattern) => Ok(Json(pattern)),
        None => Err(AppError(
            StatusCode::NOT_FOUND,
            format!("pattern '{}' not found", id),
        )),
    }
}

pub async fn delete_pattern_handler(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<Json<DeleteResponse>> {
    let db = state.db.clone();
    let id_for_delete = id.clone();
    let deleted = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::delete_pattern(&conn, &id_for_delete)
    })
    .await??;

    if deleted {
        Ok(Json(DeleteResponse { deleted }))
    } else {
        Err(AppError(
            StatusCode::NOT_FOUND,
            format!("pattern '{}' not found", id),
        ))
    }
}

pub async fn list_pattern_comments_handler(
    State(state): State<AppState>,
    Path(id): Path<String>,
) -> Result<Json<Vec<db::PatternComment>>> {
    let db = state.db.clone();
    let id_for_lookup = id.clone();
    let comments = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_pattern_comments(&conn, &id_for_lookup)
    })
    .await??;

    match comments {
        Some(comments) => Ok(Json(comments)),
        None => Err(AppError(
            StatusCode::NOT_FOUND,
            format!("pattern '{}' not found", id),
        )),
    }
}

pub async fn add_pattern_comment_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<String>,
    Json(req): Json<AddCommentRequest>,
) -> Result<Json<db::PatternComment>> {
    if req.content.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "content is required".into(),
        ));
    }
    let author = resolve_identity(req.author, &headers);
    let author_type = req.author_type.unwrap_or_else(|| {
        if actor_agent_id(&headers).is_some() {
            "agent".to_string()
        } else {
            "user".to_string()
        }
    });
    if author_type != "agent" && author_type != "user" && author_type != "system" {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            format!("invalid author_type '{author_type}': must be agent|user|system"),
        ));
    }

    let db = state.db.clone();
    let id_for_insert = id.clone();
    let content = req.content;
    let comment = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::insert_pattern_comment(&conn, &id_for_insert, &author, &author_type, &content)
    })
    .await??;

    match comment {
        Some(comment) => Ok(Json(comment)),
        None => Err(AppError(
            StatusCode::NOT_FOUND,
            format!("pattern '{}' not found", id),
        )),
    }
}

// ── Tasks API ────────────────────────────────────────────────────────────────

fn present_nullable_value<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> std::result::Result<Option<Value>, D::Error> {
    Value::deserialize(deserializer).map(Some)
}

#[derive(Deserialize)]
pub struct ListTasksQuery {
    /// Comma-separated list of statuses (e.g. "todo,in_progress").
    /// Defaults to `todo,in_progress` when absent.
    pub status: Option<String>,
    /// When true, include `done` tasks older than 7 days. Default false.
    pub include_stale: Option<bool>,
    /// Exact goal; an empty value selects tasks without a goal.
    pub goal: Option<String>,
    pub all_goals: Option<bool>,
}

#[derive(Deserialize)]
pub struct CreateTaskRequest {
    pub requester: Option<crate::task_dispatch::Target>,
    pub canonical_remote: Option<String>,
    #[serde(default)]
    pub routing: crate::task_targets::Routing,
    pub title: String,
    #[serde(default, deserialize_with = "present_nullable_value")]
    pub goal: Option<Value>,
    pub description: Option<String>,
    pub specification: Option<String>,
    pub details: Option<String>,
    pub labels: Option<Vec<String>>,
    pub hostname: Option<String>,
    /// Optional override of reporter. Defaults to X-Agent-Id header, or "user"
    /// when the header is absent or "_default".
    pub reporter: Option<String>,
}

#[derive(Deserialize)]
pub struct DelegateTaskRequest {
    pub requester: Option<crate::task_dispatch::Target>,
    pub target_project_ident: String,
    pub title: String,
    pub description: Option<String>,
    pub details: Option<String>,
    pub specification: Option<String>,
    pub labels: Option<Vec<String>>,
    pub hostname: Option<String>,
    pub reporter: Option<String>,
}

// Preserve explicit JSON null so owner clearing still passes ownership checks.

#[derive(Deserialize)]
pub struct UpdateTaskRequest {
    #[serde(default, deserialize_with = "present_nullable_value")]
    pub goal: Option<Value>,
    pub status: Option<String>,
    /// `Some(null)` in JSON clears the owner; `Some("xyz")` assigns it;
    /// absent leaves the current owner alone.
    #[serde(default, deserialize_with = "present_nullable_value")]
    pub owner_agent_id: Option<serde_json::Value>,
    pub rank: Option<i64>,
    pub title: Option<String>,
    pub description: Option<serde_json::Value>,
    pub specification: Option<serde_json::Value>,
    pub details: Option<serde_json::Value>,
    pub labels: Option<Vec<String>>,
    pub hostname: Option<serde_json::Value>,
}

#[derive(Deserialize)]
pub struct UpdateDelegationRequest {
    pub title: Option<String>,
    pub description: Option<Option<String>>,
    pub details: Option<Option<String>>,
    pub specification: Option<Option<String>>,
    pub labels: Option<Vec<String>>,
}

#[derive(Deserialize)]
pub struct AddCommentRequest {
    pub content: String,
    /// Display provenance, never used for authorization or routing.
    pub hostname: Option<String>,
    /// `"agent"` | `"user"`. Defaults based on whether X-Agent-Id is present.
    pub author_type: Option<String>,
    /// Defaults to X-Agent-Id header, or `"user"` when the header is absent or
    /// `"_default"`.
    pub author: Option<String>,
}

/// Flat detail shape: all `Task` fields at the top level (via `#[serde(flatten)]`)
/// plus a sibling `comments` array and a derived `actions` array. Designed so
/// that ndesign's `data-nd-bind` can render the detail view — including
/// status-transition buttons — without template conditionals.
#[derive(Serialize)]
pub struct TaskWithComments {
    #[serde(flatten)]
    pub task: db::Task,
    pub specification: Option<String>,
    pub comments: Vec<db::TaskComment>,
    /// Total comments on the task; `comments` may hold only the newest of them.
    pub comment_count: i64,
    pub actions: Vec<TaskAction>,
}

impl TaskWithComments {
    fn new(task: db::Task, comments: Vec<db::TaskComment>, comment_count: i64) -> Self {
        let specification = task.details.clone();
        let actions = actions_for_status(&task.status);
        Self {
            task,
            specification,
            comments,
            comment_count,
            actions,
        }
    }
}

#[derive(Debug, Default, Deserialize)]
pub struct GetTaskQuery {
    /// Return only the newest N comments; omitted returns all of them.
    pub comments: Option<usize>,
}

const TASK_SPECIFICATION_HINT: &str = "If this is a complex task, make sure it has a proper specification so another agent can pick up the work if you drop it.";

#[derive(Serialize)]
pub struct TaskCreateResponse {
    #[serde(flatten)]
    pub task: db::Task,
    pub specification: Option<String>,
    pub hint: &'static str,
}

#[derive(Serialize)]
pub struct DelegationResponse {
    pub delegation: db::TaskDelegation,
    pub source_task: db::Task,
    pub target_task: db::Task,
    pub message_id: i64,
}

impl TaskCreateResponse {
    fn new(task: db::Task) -> Self {
        let specification = task.details.clone();
        Self {
            task,
            specification,
            hint: TASK_SPECIFICATION_HINT,
        }
    }
}

fn system_nudge(
    conn: &rusqlite::Connection,
    project_ident: &str,
    subject: &str,
    content: String,
) -> anyhow::Result<i64> {
    db::insert_message(
        conn,
        &Message {
            id: 0,
            project_ident: project_ident.to_string(),
            source: "system".into(),
            external_message_id: None,
            content,
            sent_at: now_ms(),
            confirmed_at: None,
            parent_message_id: None,
            agent_id: None,
            message_type: "message".into(),
            subject: Some(subject.to_string()),
            hostname: Some("agent-gateway".into()),
            event_at: Some(now_ms()),
            deliver_to_agents: true,
            author_kind: Some("system".to_string()),
            resolved_at: None,
            resolved_by: None,
        },
    )
}

/// One status-transition button derived from the task's current status.
///
/// The UI iterates this array inside the modal; each entry is rendered as a
/// `<button data-nd-action="PATCH …" data-nd-body=…>` so ndesign fires the
/// PATCH when the user clicks. `style` is the `nd-btn-*` suffix
/// (`primary` | `secondary`) so the template can build the class name.
#[derive(Serialize)]
pub struct TaskAction {
    pub verb: String,
    pub style: String,
    pub target_status: String,
}

/// Compute the list of allowed status transitions for a given current status.
/// Kept in one place so the UI and any future API consumers agree.
fn actions_for_status(status: &str) -> Vec<TaskAction> {
    let mk = |verb: &str, style: &str, target: &str| TaskAction {
        verb: verb.into(),
        style: style.into(),
        target_status: target.into(),
    };
    match status {
        "todo" => vec![
            mk("Claim", "primary", "in_progress"),
            mk("Done", "primary", "done"),
            mk("User review", "secondary", "user_review"),
        ],
        "in_progress" => vec![
            mk("Release", "secondary", "todo"),
            mk("Done", "primary", "done"),
            mk("User review", "secondary", "user_review"),
        ],
        "user_review" => vec![
            mk("Return to TODO", "secondary", "todo"),
            mk("Close", "primary", "done"),
        ],
        "done" => vec![mk("Reopen", "secondary", "todo")],
        _ => vec![],
    }
}

#[derive(Serialize)]
pub struct DeleteResponse {
    pub deleted: bool,
}

#[derive(Deserialize)]
pub struct ReorderTasksQuery {
    pub goal: Option<String>,
    /// Target column (`todo` | `in_progress` | `user_review` | `done`). Required.
    pub status: String,
}

#[derive(Deserialize)]
pub struct ReorderTasksRequest {
    pub order: Vec<String>,
}

/// Parse a JSON nullable-string update field.
///
/// - `None`                          → `None`        (field not touched)
/// - `Some(Value::Null)`             → `Some(None)`  (clear column)
/// - `Some(Value::String(s))`        → `Some(Some(s))` (set column)
/// - anything else                   → 400
fn decode_nullable_string(
    field: &str,
    value: Option<serde_json::Value>,
) -> Result<Option<Option<String>>> {
    match value {
        None => Ok(None),
        Some(serde_json::Value::Null) => Ok(Some(None)),
        Some(serde_json::Value::String(s)) => Ok(Some(Some(s))),
        Some(_) => Err(AppError(
            StatusCode::BAD_REQUEST,
            format!("'{field}' must be a string or null"),
        )),
    }
}

/// Resolve the reporter/author identity from an explicit body field, the
/// X-Agent-Id header, or fall back to `"user"`.
pub(crate) fn resolve_identity(explicit: Option<String>, headers: &HeaderMap) -> String {
    if let Some(s) = explicit.and_then(|s| {
        let t = s.trim().to_string();
        if t.is_empty() {
            None
        } else {
            Some(t)
        }
    }) {
        return s;
    }
    let hdr = extract_agent_id(headers);
    if hdr == "_default" {
        crate::ui_auth::request_user(headers)
            .map(|u| u.username)
            .unwrap_or_else(|| "user".to_string())
    } else {
        hdr
    }
}

/// Optional agent id from header for actor-aware operations (None when the
/// header is absent or is the sentinel "_default").
fn actor_agent_id(headers: &HeaderMap) -> Option<String> {
    let hdr = extract_agent_id(headers);
    if hdr == "_default" {
        None
    } else {
        Some(hdr)
    }
}

#[derive(Debug)]
struct TaskGoalConflict;
impl std::fmt::Display for TaskGoalConflict {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("select the task's goal with X-Task-Goal before claiming or changing its goal")
    }
}
impl std::error::Error for TaskGoalConflict {}

/// Explicit goal context opts an agent into a goal's queue.
fn task_goal_header(headers: &HeaderMap) -> Option<String> {
    headers
        .get("x-task-goal")
        .and_then(|v| std::str::from_utf8(v.as_bytes()).ok())
        .map(|s| s.trim().to_string())
}

pub async fn list_tasks_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    Query(q): Query<ListTasksQuery>,
) -> Result<Json<Vec<db::TaskSummary>>> {
    // Verify project exists (consistent with other handlers).
    {
        let conn = state.db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            return Err(AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            ));
        }
    }

    let statuses: Vec<String> = match q.status.as_deref() {
        None | Some("") => vec!["todo".into(), "in_progress".into()],
        Some(s) => s
            .split(',')
            .map(|p| p.trim().to_string())
            .filter(|p| !p.is_empty())
            .collect(),
    };
    for s in &statuses {
        if !matches!(
            s.as_str(),
            "todo" | "in_progress" | "user_review" | "done" | "cancelled"
        ) {
            return Err(AppError(
                StatusCode::BAD_REQUEST,
                format!(
                    "invalid status '{s}': must be todo|in_progress|user_review|done|cancelled"
                ),
            ));
        }
    }
    let include_stale = q.include_stale.unwrap_or(false);
    let agent =
        actor_agent_id(&headers).is_some() || crate::task_events::origin(&headers)?.is_some();
    let selected_goal = q.goal.or_else(|| task_goal_header(&headers));
    let filter_goal = !q.all_goals.unwrap_or(false) && (selected_goal.is_some() || agent);

    let db = state.db.clone();
    let ident_for_list = ident;
    let tasks = spawn_blocking(move || -> anyhow::Result<Vec<db::TaskSummary>> {
        let conn = db.lock().unwrap();
        let mut tasks = db::list_tasks(&conn, &ident_for_list, &statuses, include_stale)?;
        if filter_goal {
            let goal = selected_goal.as_deref().unwrap_or("").trim();
            tasks.retain(|task| task.goal.as_deref().unwrap_or("") == goal);
        }
        Ok(tasks)
    })
    .await??;

    Ok(Json(tasks))
}

/// An agent submitter must identify its session; reporter/display labels are not owners.
pub(crate) fn task_creation_origin(
    headers: &HeaderMap,
) -> Result<Option<crate::task_events::AgentOrigin>> {
    let origin = crate::task_events::origin(headers)?;
    if actor_agent_id(headers).is_some() && origin.is_none() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "agent task creation requires complete X-Agent session/instance/provider/OS headers"
                .into(),
        ));
    }
    Ok(origin)
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReassignTaskRequest {
    pub target: crate::task_dispatch::Target,
}

/// The exact current owning agent may reassign an ordinary active task.
pub async fn reassign_task_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((project, id)): Path<(String, String)>,
    Json(req): Json<ReassignTaskRequest>,
) -> Result<Json<db::Task>> {
    let origin = crate::task_events::origin(&headers)?.ok_or_else(|| {
        AppError(
            StatusCode::BAD_REQUEST,
            "exact calling agent origin required".into(),
        )
    })?;
    let database = state.db.clone();
    let task = spawn_blocking(move || -> anyhow::Result<Option<db::Task>> {
        let conn = database.lock().unwrap();
        crate::task_events::mutation(&conn, Some(&origin), |conn| {
            db::reassign_task(conn, &project, &id, &req.target)
        })
    })
    .await??;
    task.map(Json)
        .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "task not found".into()))
}

/// Desktop user assignment uses the existing bearer connection, with no agent origin.
pub async fn assign_task_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((project, id)): Path<(String, String)>,
    Json(req): Json<ReassignTaskRequest>,
) -> Result<Json<db::Task>> {
    if crate::task_events::origin(&headers)?.is_some() || actor_agent_id(&headers).is_some() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "human assignment must not send agent identity headers".into(),
        ));
    }
    let task = spawn_blocking(move || -> anyhow::Result<Option<db::Task>> {
        let conn = state.db.lock().unwrap();
        crate::task_events::mutation(&conn, None, |conn| {
            db::assign_task_from_user(conn, &project, &id, &req.target)
        })
    })
    .await??;
    task.map(Json)
        .ok_or_else(|| AppError(StatusCode::NOT_FOUND, "task not found".into()))
}

pub async fn create_task_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    Json(req): Json<CreateTaskRequest>,
) -> Result<Json<TaskCreateResponse>> {
    let origin = task_creation_origin(&headers)?;
    // Verify project exists.
    {
        let conn = state.db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            return Err(AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            ));
        }
    }

    let title = req.title.trim().to_string();
    if title.is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "'title' must be non-empty".into(),
        ));
    }

    let reporter = resolve_identity(req.reporter, &headers);
    let description = req.description;
    let specification = req.specification.or(req.details);
    let labels = req.labels.unwrap_or_default();
    let hostname = req.hostname;
    let goal = match decode_nullable_string("goal", req.goal)? {
        Some(goal) => goal,
        None => task_goal_header(&headers),
    };
    let goal = db::normalize_task_goal(goal.as_deref())
        .map_err(|e| AppError(StatusCode::BAD_REQUEST, e.to_string()))?;

    let db = state.db.clone();
    let ident_clone = ident;
    let task = spawn_blocking(move || -> Result<db::Task> {
        let conn = db.lock().unwrap();
        if !matches!(req.routing, crate::task_targets::Routing::Auto)
            || req.canonical_remote.is_some()
        {
            crate::task_targets::require_binding(
                &conn,
                &ident_clone,
                req.canonical_remote.as_deref(),
            )?;
        }
        Ok(crate::task_events::mutation(
            &conn,
            origin.as_ref(),
            |conn| {
                let task = db::insert_task_with_requester(
                    conn,
                    &ident_clone,
                    &title,
                    description.as_deref(),
                    specification.as_deref(),
                    &labels,
                    hostname.as_deref(),
                    &reporter,
                    goal.as_deref(),
                    &req.routing,
                    req.requester.as_ref(),
                )?;
                if task.goal.is_none()
                    && task.owner_agent_id.is_none()
                    && task.status == "todo"
                    && !matches!(task.routing, crate::task_targets::Routing::Unassigned)
                {
                    system_nudge(
                        conn,
                        &ident_clone,
                        "Task created",
                        crate::templates::render(
                            crate::templates::TASK_CREATED,
                            &[
                                ("task_id", &task.id),
                                ("project", &ident_clone),
                                ("title", &task.title),
                            ],
                        ),
                    )?;
                }
                Ok(task)
            },
        )?)
    })
    .await??;

    Ok(Json(TaskCreateResponse::new(task)))
}

pub async fn delegate_task_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(source_ident): Path<String>,
    Json(req): Json<DelegateTaskRequest>,
) -> Result<Json<DelegationResponse>> {
    let origin = task_creation_origin(&headers)?;
    let source_ident = normalize_project_ident(&source_ident);
    let target_ident = normalize_project_ident(&req.target_project_ident);
    let title = req.title.trim().to_string();
    if target_ident.is_empty() || title.is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "'target_project_ident' and 'title' must be non-empty".into(),
        ));
    }
    if target_ident == source_ident {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "delegated task target must be a different project".into(),
        ));
    }

    let reporter = resolve_identity(req.reporter, &headers);
    let requester = actor_agent_id(&headers);
    let description = req.description;
    let specification = req.specification.or(req.details);
    let labels = req.labels.unwrap_or_default();
    let hostname = req.hostname;

    let db = state.db.clone();
    let response = spawn_blocking(move || -> anyhow::Result<DelegationResponse> {
        let conn = db.lock().unwrap();
        crate::task_events::mutation(&conn, origin.as_ref(), |conn| {
            if db::get_project(conn, &source_ident)?.is_none() {
                anyhow::bail!("project '{source_ident}' not found");
            }
            if db::get_project(conn, &target_ident)?.is_none() {
                anyhow::bail!("project '{target_ident}' not found");
            }

            let target_task = db::insert_task(
                conn,
                &target_ident,
                &title,
                description.as_deref(),
                specification.as_deref(),
                &labels,
                hostname.as_deref(),
                &reporter,
            )?;
            let source_title = format!("{title} (DELEGATED)");
            let source_task = db::insert_delegated_task_with_requester(
                conn,
                &db::DelegatedTaskInsert {
                    project_ident: &source_ident,
                    title: &source_title,
                    description: description.as_deref(),
                    details: specification.as_deref(),
                    labels: &labels,
                    hostname: hostname.as_deref(),
                    reporter: &reporter,
                    target_project_ident: &target_ident,
                    target_task_id: &target_task.id,
                },
                req.requester.as_ref(),
            )?;
            let delegation = db::insert_task_delegation(
                conn,
                &source_ident,
                &source_task.id,
                &target_ident,
                &target_task.id,
                requester.as_deref(),
                hostname.as_deref(),
            )?;
            db::insert_comment(
                conn,
                &source_task.id,
                "agent-gateway",
                "system",
                &format!(
                    "Delegated to project `{}` as task `{}`.",
                    target_ident, target_task.id
                ),
            )?;
            db::insert_comment(
                conn,
                &target_task.id,
                "agent-gateway",
                "system",
                &format!(
                    "Delegated from project `{}`; source tracking task `{}`.",
                    source_ident, source_task.id
                ),
            )?;
            let message_id = system_nudge(
                conn,
                &target_ident,
                "Delegated task created",
                crate::templates::render(
                    crate::templates::DELEGATED_CREATED,
                    &[
                        ("source_project", &source_ident),
                        ("target_task_id", &target_task.id),
                        ("title", &title),
                        ("target_project", &target_ident),
                    ],
                ),
            )?;

            Ok(DelegationResponse {
                delegation,
                source_task,
                target_task,
                message_id,
            })
        })
    })
    .await??;

    Ok(Json(response))
}

pub async fn get_task_handler(
    State(state): State<AppState>,
    Path((ident, task_id)): Path<(String, String)>,
    Query(q): Query<GetTaskQuery>,
) -> Result<Json<TaskWithComments>> {
    let db = state.db.clone();
    let ident_for_fetch = ident.clone();
    let task_id_clone = task_id;
    let detail = spawn_blocking(move || -> anyhow::Result<Option<(db::TaskDetail, i64)>> {
        let conn = db.lock().unwrap();
        let Some(detail) =
            db::get_task_detail_recent(&conn, &ident_for_fetch, &task_id_clone, q.comments)?
        else {
            return Ok(None);
        };
        let total = db::task_comment_count(&conn, &task_id_clone)?;
        Ok(Some((detail, total)))
    })
    .await??;

    match detail {
        Some((d, total)) => Ok(Json(TaskWithComments::new(d.task, d.comments, total))),
        None => Err(AppError(
            StatusCode::NOT_FOUND,
            format!("task not found in project '{}'", ident),
        )),
    }
}

pub async fn update_task_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((ident, task_id)): Path<(String, String)>,
    Json(req): Json<UpdateTaskRequest>,
) -> Result<Json<db::Task>> {
    let origin = crate::task_events::origin(&headers)?;
    // Validate & decode the nullable-string fields up-front so we can return
    // 400 on a client-side shape mistake rather than bubbling a 500.
    let goal_opt = decode_nullable_string("goal", req.goal)?
        .map(|goal| db::normalize_task_goal(goal.as_deref()))
        .transpose()
        .map_err(|e| AppError(StatusCode::BAD_REQUEST, e.to_string()))?;
    let owner_opt = decode_nullable_string("owner_agent_id", req.owner_agent_id)?;
    let description_opt = decode_nullable_string("description", req.description)?;
    let details_opt = if req.specification.is_some() {
        decode_nullable_string("specification", req.specification)?
    } else {
        decode_nullable_string("details", req.details)?
    };
    let hostname_opt = decode_nullable_string("hostname", req.hostname)?;

    let actor = actor_agent_id(&headers);
    let agent_request = actor.is_some() || origin.is_some();
    let selected_goal = task_goal_header(&headers);
    let db = state.db.clone();
    let ident_for_update = ident.clone();
    let task_id_clone = task_id;
    let status = req.status;
    let rank = req.rank;
    let title = req.title;
    let labels = req.labels;

    // Invalid status transitions and bad status values are currently reported
    // by `db::update_task` as anyhow errors; they bubble through `AppError`'s
    // blanket From impl as 500. The CHECK constraint on `status` catches the
    // truly invalid values at the SQL layer. Refine to 400 when we add a
    // dedicated error enum.
    let task = spawn_blocking(move || -> anyhow::Result<Option<db::Task>> {
        let conn = db.lock().unwrap();
        crate::task_events::mutation(&conn, origin.as_ref(), |conn| {
            let before =
                db::get_task_detail(conn, &ident_for_update, &task_id_clone)?.map(|d| d.task);

            if agent_request {
                if let Some(goal) = before.as_ref().and_then(|task| task.goal.as_deref()) {
                    let changes_queue = status.as_deref() == Some("in_progress")
                        || owner_opt.as_ref().is_some_and(|o| o.is_some())
                        || goal_opt.is_some();
                    if changes_queue && selected_goal.as_deref() != Some(goal) {
                        return Err(TaskGoalConflict.into());
                    }
                }
            }
            let upd = db::TaskUpdate {
                goal: goal_opt.as_ref().map(|g| g.as_deref()),
                status: status.as_deref(),
                owner_agent_id: owner_opt.as_ref().map(|inner| inner.as_deref()),
                rank,
                title: title.as_deref(),
                description: description_opt.as_ref().map(|inner| inner.as_deref()),
                details: details_opt.as_ref().map(|inner| inner.as_deref()),
                labels: labels.as_deref(),
                hostname: hostname_opt.as_ref().map(|inner| inner.as_deref()),
            };
            let updated = if agent_request {
                db::update_task(
                    conn,
                    &ident_for_update,
                    &task_id_clone,
                    &upd,
                    actor.as_deref(),
                )?
            } else {
                db::update_task_from_user(conn, &ident_for_update, &task_id_clone, &upd)?
            };

            if let (Some(before), Some(after)) = (&before, &updated) {
                if before.status != "done" && after.status == "done" {
                    if let Some(delegation) =
                        db::get_delegation_by_target(conn, &ident_for_update, &task_id_clone)?
                    {
                        let target_detail = db::get_task_detail(
                            conn,
                            &delegation.target_project_ident,
                            &delegation.target_task_id,
                        )?
                        .ok_or_else(|| anyhow::anyhow!("target delegated task missing"))?;
                        let comments = target_detail
                            .comments
                            .iter()
                            .filter(|c| c.author_type != "system")
                            .map(|c| {
                                crate::templates::render(
                                    crate::templates::DELEGATED_COMPLETED_COMMENT,
                                    &[("author", &c.author), ("content", &c.content)],
                                )
                            })
                            .collect::<Vec<_>>()
                            .join("\n");
                        let summary = crate::templates::render(
                            crate::templates::DELEGATED_COMPLETED,
                            &[
                                ("target_project", &delegation.target_project_ident),
                                ("title", &target_detail.task.title),
                                (
                                    "description",
                                    target_detail.task.description.as_deref().unwrap_or(""),
                                ),
                                (
                                    "specification",
                                    target_detail.task.details.as_deref().unwrap_or(""),
                                ),
                                (
                                    "comments",
                                    if comments.is_empty() {
                                        "(none)"
                                    } else {
                                        &comments
                                    },
                                ),
                            ],
                        );
                        db::insert_comment(
                            conn,
                            &delegation.source_task_id,
                            "agent-gateway",
                            "system",
                            &summary,
                        )?;
                        let done_upd = db::TaskUpdate {
                            goal: None,
                            status: Some("done"),
                            owner_agent_id: None,
                            rank: None,
                            title: None,
                            description: None,
                            details: None,
                            labels: None,
                            hostname: None,
                        };
                        let _ = db::update_task_mirrored(
                            conn,
                            &delegation.source_project_ident,
                            &delegation.source_task_id,
                            &done_upd,
                        )?
                        .ok_or_else(|| anyhow::anyhow!("source delegated task missing"))?;
                        let message_id = system_nudge(
                            conn,
                            &delegation.source_project_ident,
                            "Delegated task completed",
                            summary,
                        )?;
                        db::mark_delegation_complete(conn, &delegation.id, message_id)?;
                    }
                }
            }

            Ok(updated)
        })
    })
    .await??;

    match task {
        Some(t) => Ok(Json(t)),
        None => Err(AppError(
            StatusCode::NOT_FOUND,
            format!("task not found in project '{}'", ident),
        )),
    }
}

pub async fn update_delegation_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((ident, task_id)): Path<(String, String)>,
    Json(req): Json<UpdateDelegationRequest>,
) -> Result<Json<DelegationResponse>> {
    let origin = crate::task_events::origin(&headers)?;
    let details_opt = if req.specification.is_some() {
        req.specification
    } else {
        req.details
    };
    let db = state.db.clone();
    let response = spawn_blocking(move || -> anyhow::Result<DelegationResponse> {
        let conn = db.lock().unwrap();
        crate::task_events::mutation(&conn, origin.as_ref(), |conn| {
            let delegation =
                db::get_delegation_by_source(conn, &ident, &task_id)?.ok_or_else(|| {
                    anyhow::anyhow!("task '{task_id}' is not a delegated source task")
                })?;
            let source_title = req.title.as_ref().map(|title| {
                if title.ends_with(" (DELEGATED)") {
                    title.clone()
                } else {
                    format!("{title} (DELEGATED)")
                }
            });

            let source_upd = db::TaskUpdate {
                goal: None,
                status: None,
                owner_agent_id: None,
                rank: None,
                title: source_title.as_deref(),
                description: req.description.as_ref().map(|inner| inner.as_deref()),
                details: details_opt.as_ref().map(|inner| inner.as_deref()),
                labels: req.labels.as_deref(),
                hostname: None,
            };
            let source_task = db::update_task(
                conn,
                &delegation.source_project_ident,
                &delegation.source_task_id,
                &source_upd,
                None,
            )?
            .ok_or_else(|| anyhow::anyhow!("source delegated task missing"))?;

            let target_title = req
                .title
                .as_deref()
                .map(|t| t.trim_end_matches(" (DELEGATED)"));
            let target_upd = db::TaskUpdate {
                goal: None,
                status: None,
                owner_agent_id: None,
                rank: None,
                title: target_title,
                description: req.description.as_ref().map(|inner| inner.as_deref()),
                details: details_opt.as_ref().map(|inner| inner.as_deref()),
                labels: req.labels.as_deref(),
                hostname: None,
            };
            let target_task = db::update_task(
                conn,
                &delegation.target_project_ident,
                &delegation.target_task_id,
                &target_upd,
                None,
            )?
            .ok_or_else(|| anyhow::anyhow!("target delegated task missing"))?;

            let note = "Delegated task contract was updated by the source project.";
            db::insert_comment(
                conn,
                &delegation.source_task_id,
                "agent-gateway",
                "system",
                note,
            )?;
            db::insert_comment(
                conn,
                &delegation.target_task_id,
                "agent-gateway",
                "system",
                note,
            )?;
            let message_id = system_nudge(
                conn,
                &delegation.target_project_ident,
                "Delegated task updated",
                crate::templates::render(
                    crate::templates::DELEGATED_UPDATED,
                    &[
                        ("source_project", &delegation.source_project_ident),
                        ("target_task_id", &delegation.target_task_id),
                    ],
                ),
            )?;

            Ok(DelegationResponse {
                delegation,
                source_task,
                target_task,
                message_id,
            })
        })
    })
    .await??;

    Ok(Json(response))
}

pub async fn add_comment_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((ident, task_id)): Path<(String, String)>,
    Json(req): Json<AddCommentRequest>,
) -> Result<Json<db::TaskComment>> {
    let origin = crate::task_events::origin(&headers)?;
    let content = req.content;
    if content.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "'content' must be non-empty".into(),
        ));
    }

    // Confirm the task exists in the project first.
    {
        let conn = state.db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            return Err(AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            ));
        }
        if db::get_task_detail(&conn, &ident, &task_id)?.is_none() {
            return Err(AppError(
                StatusCode::NOT_FOUND,
                format!("task not found in project '{}'", ident),
            ));
        }
    }

    let header_agent = actor_agent_id(&headers);
    let author = resolve_identity(req.author, &headers);
    let author_type = match req.author_type.as_deref() {
        Some("agent") => "agent".to_string(),
        Some("user") => "user".to_string(),
        Some(other) => {
            return Err(AppError(
                StatusCode::BAD_REQUEST,
                format!("invalid author_type '{other}': must be agent|user"),
            ));
        }
        None => {
            if header_agent.is_some() {
                "agent".into()
            } else {
                "user".into()
            }
        }
    };

    let db = state.db.clone();
    let ident_clone = ident;
    let task_id_clone = task_id;
    let comment = spawn_blocking(move || -> anyhow::Result<db::TaskComment> {
        let conn = db.lock().unwrap();
        crate::task_events::mutation(&conn, origin.as_ref(), |conn| {
            let comment = db::insert_comment_with_hostname(
                conn,
                &task_id_clone,
                &author,
                &author_type,
                &content,
                req.hostname.as_deref(),
            )?;
            if author_type != "system" {
                if let Some(delegation) =
                    db::get_delegation_by_target(conn, &ident_clone, &task_id_clone)?
                {
                    let body = crate::templates::render(
                        crate::templates::DELEGATED_COMMENT,
                        &[
                            ("source_task_id", &delegation.source_task_id),
                            ("target_project", &delegation.target_project_ident),
                            ("author", &author),
                            ("content", &content),
                        ],
                    );
                    system_nudge(
                        conn,
                        &delegation.source_project_ident,
                        "Delegated task comment added",
                        body,
                    )?;
                }
            }
            Ok(comment)
        })
    })
    .await??;

    Ok(Json(comment))
}

pub async fn delete_task_handler(
    State(state): State<AppState>,
    Path((ident, task_id)): Path<(String, String)>,
) -> Result<Json<DeleteResponse>> {
    let db = state.db.clone();
    let ident_clone = ident;
    let task_id_clone = task_id;
    let deleted = spawn_blocking(move || -> anyhow::Result<bool> {
        let conn = db.lock().unwrap();
        db::delete_task(&conn, &ident_clone, &task_id_clone)
    })
    .await??;
    Ok(Json(DeleteResponse { deleted }))
}

/// Apply a client-driven reorder within a single status column.
///
/// Designed to receive `data-nd-sortable` POSTs: the body is
/// `{"order": ["id1", "id2", ...]}` and `?status=` selects the column the
/// order applies to. Returns the fresh list for that column so callers can
/// re-render without a follow-up GET.
pub async fn reorder_tasks_handler(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
    axum::extract::Query(q): axum::extract::Query<ReorderTasksQuery>,
    Json(req): Json<ReorderTasksRequest>,
) -> Result<Json<Vec<db::TaskSummary>>> {
    let origin = crate::task_events::origin(&headers)?;
    // Verify project exists (consistent with other project-scoped handlers).
    {
        let conn = state.db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            return Err(AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            ));
        }
    }

    let status = q.status.clone();
    if !matches!(
        status.as_str(),
        "todo" | "in_progress" | "user_review" | "done"
    ) {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            format!("invalid status '{status}': must be todo|in_progress|user_review|done"),
        ));
    }

    let actor = actor_agent_id(&headers);
    let agent_request = actor.is_some() || origin.is_some();
    let selected_goal = task_goal_header(&headers);
    let response_goal = q.goal.or_else(|| selected_goal.clone());
    let filter_goal = agent_request || response_goal.is_some();
    let db = state.db.clone();
    let ident_clone = ident;
    let status_clone = status.clone();
    let order = req.order;

    let tasks = spawn_blocking(move || -> anyhow::Result<Vec<db::TaskSummary>> {
        let conn = db.lock().unwrap();
        crate::task_events::mutation(&conn, origin.as_ref(), |conn| {
            if agent_request && status_clone == "in_progress" {
                for id in &order {
                    if let Some(detail) = db::get_task_detail(conn, &ident_clone, id)? {
                        if let Some(goal) = detail.task.goal.as_deref() {
                            if selected_goal.as_deref() != Some(goal) {
                                return Err(TaskGoalConflict.into());
                            }
                        }
                    }
                }
            }
            db::reorder_tasks_in_column(conn, &ident_clone, &status_clone, &order, actor.as_deref())
        })?;
        let mut tasks = db::list_tasks(
            &conn,
            &ident_clone,
            std::slice::from_ref(&status_clone),
            false,
        )?;
        if filter_goal {
            let goal = response_goal.as_deref().unwrap_or("").trim();
            tasks.retain(|task| task.goal.as_deref().unwrap_or("") == goal);
        }
        Ok(tasks)
    })
    .await??;

    Ok(Json(tasks))
}

// ── GET /v1/projects (JSON — used by the Tasks picker binding) ───────────────

/// List all registered projects with the same per-project stats shape the
/// dashboard uses. Returned as a bare array (no envelope) so ndesign's
/// `data-nd-bind` can render rows directly.
pub async fn list_projects_handler(
    State(state): State<AppState>,
) -> Result<Json<Vec<db::ProjectStats>>> {
    let db = state.db.clone();
    let projects = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_project_stats(&conn)
    })
    .await??;
    Ok(Json(projects))
}

// ── GET /patterns (global pattern library) ───────────────────────────────────

// ── GET /patterns/:id (global pattern detail/editor) ─────────────────────────

// ── GET /tasks (project picker) ──────────────────────────────────────────────

// ── GET /task-link/:task_ref (dedicated task detail) ─────────────────────────

// ── GET /projects/:ident/tasks (board) ───────────────────────────────────────

/// `<head>` extras shared by every page that renders the task board: the
/// `selectedTaskId` store var for the detail modal plus card CSS.

// ── Login / logout / users ───────────────────────────────────────────────────

#[derive(Deserialize, Default)]
pub struct LoginQuery {
    pub next: Option<String>,
    pub error: Option<String>,
}

#[derive(Deserialize)]
pub struct LoginForm {
    pub username: Option<String>,
    pub password: Option<String>,
    pub next: Option<String>,
    /// Bootstrap-only fields: creating the first admin requires the API key.
    pub api_key: Option<String>,
    pub display_name: Option<String>,
}

// ── Users API ────────────────────────────────────────────────────────────────
//
// Reachable with the bearer key (agents / CLI act as administrators) or with
// an admin browser session. Members may only change their own password.

#[derive(Serialize)]
pub struct UserView {
    pub id: i64,
    pub username: String,
    pub display_name: String,
    pub role: String,
    pub disabled: bool,
    pub created_at: i64,
    pub last_login_at: Option<i64>,
}

impl From<db::User> for UserView {
    fn from(u: db::User) -> Self {
        UserView {
            id: u.id,
            username: u.username,
            display_name: u.display_name,
            role: u.role,
            disabled: u.disabled_at.is_some(),
            created_at: u.created_at,
            last_login_at: u.last_login_at,
        }
    }
}

#[derive(Deserialize)]
pub struct CreateUserRequest {
    pub username: String,
    pub password: String,
    pub display_name: Option<String>,
    pub role: Option<String>,
}

#[derive(Deserialize, Default)]
pub struct UpdateUserRequest {
    pub display_name: Option<String>,
    pub role: Option<String>,
    /// Accepts booleans or the strings ndesign forms send ("true"/"on").
    pub disabled: Option<Value>,
    pub password: Option<String>,
}

#[derive(Deserialize)]
pub struct ChangePasswordRequest {
    pub current_password: String,
    pub new_password: String,
}

/// Bearer-key callers are administrators; browser sessions must be admins.
pub(crate) fn require_admin(headers: &HeaderMap) -> Result<()> {
    match crate::ui_auth::request_user(headers) {
        None => Ok(()),
        Some(user) if user.is_admin() => Ok(()),
        Some(_) => Err(AppError(
            StatusCode::FORBIDDEN,
            "administrator role required".into(),
        )),
    }
}

/// GET /v1/users
pub async fn list_users(
    State(state): State<AppState>,
    headers: HeaderMap,
) -> Result<Json<Vec<UserView>>> {
    require_admin(&headers)?;
    let db = state.db.clone();
    let users = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::list_users(&conn)
    })
    .await??;
    Ok(Json(users.into_iter().map(UserView::from).collect()))
}

/// POST /v1/users
pub async fn create_user(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<CreateUserRequest>,
) -> Result<Json<UserView>> {
    require_admin(&headers)?;
    let hash = crate::ui_auth::hash_password(&req.password)
        .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?;
    let role = req
        .role
        .map(|r| r.trim().to_lowercase())
        .filter(|r| !r.is_empty())
        .unwrap_or_else(|| db::USER_ROLE_MEMBER.to_string());
    let db = state.db.clone();
    let user = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::create_user(
            &conn,
            &req.username,
            req.display_name.as_deref().unwrap_or(""),
            &role,
            &hash,
        )
    })
    .await?
    .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?;
    Ok(Json(user.into()))
}

/// PATCH /v1/users/:id
pub async fn update_user(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<i64>,
    Json(req): Json<UpdateUserRequest>,
) -> Result<Json<UserView>> {
    require_admin(&headers)?;
    let acting = crate::ui_auth::request_user(&headers);
    let new_hash = match req.password.as_deref().map(str::trim) {
        Some(p) if !p.is_empty() => Some(
            crate::ui_auth::hash_password(p)
                .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?,
        ),
        _ => None,
    };
    let disabled = req.disabled.as_ref().map(value_as_bool);
    let role = req
        .role
        .map(|r| r.trim().to_lowercase())
        .filter(|r| !r.is_empty());
    let db = state.db.clone();
    let user = spawn_blocking(move || -> anyhow::Result<db::User> {
        let conn = db.lock().unwrap();
        let existing =
            db::get_user(&conn, id)?.ok_or_else(|| anyhow::anyhow!("user {id} not found"))?;
        let demoting = matches!(role.as_deref(), Some(db::USER_ROLE_MEMBER)) && existing.is_admin();
        let disabling = disabled == Some(true) && existing.is_active();
        if (demoting || disabling) && existing.is_admin() && db::count_active_admins(&conn)? <= 1 {
            anyhow::bail!("cannot remove the last active administrator");
        }
        if let Some(acting) = acting.as_ref() {
            if acting.id == id && (demoting || disabling) {
                anyhow::bail!("you cannot demote or disable your own account");
            }
        }
        db::update_user_profile(&conn, id, req.display_name.as_deref(), role.as_deref())?;
        if let Some(hash) = new_hash.as_deref() {
            db::set_user_password(&conn, id, hash)?;
        }
        if let Some(disabled) = disabled {
            db::set_user_disabled(&conn, id, disabled)?;
        }
        db::get_user(&conn, id)?.ok_or_else(|| anyhow::anyhow!("user {id} vanished"))
    })
    .await?
    .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?;
    Ok(Json(user.into()))
}

/// DELETE /v1/users/:id
pub async fn delete_user(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(id): Path<i64>,
) -> Result<Json<ResolveResponse>> {
    require_admin(&headers)?;
    if let Some(acting) = crate::ui_auth::request_user(&headers) {
        if acting.id == id {
            return Err(AppError(
                StatusCode::BAD_REQUEST,
                "you cannot delete your own account".into(),
            ));
        }
    }
    let db = state.db.clone();
    let changed = spawn_blocking(move || -> anyhow::Result<bool> {
        let conn = db.lock().unwrap();
        if let Some(existing) = db::get_user(&conn, id)? {
            if existing.is_admin() && existing.is_active() && db::count_active_admins(&conn)? <= 1 {
                anyhow::bail!("cannot delete the last active administrator");
            }
        }
        db::delete_user(&conn, id)
    })
    .await?
    .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?;
    Ok(Json(ResolveResponse { changed }))
}

/// GET /v1/users/me — the signed-in browser user (404 for bearer callers).
pub async fn current_user(headers: HeaderMap) -> Result<Json<UserView>> {
    let user = crate::ui_auth::request_user(&headers).ok_or_else(|| {
        AppError(
            StatusCode::NOT_FOUND,
            "no browser session on this request".into(),
        )
    })?;
    Ok(Json(UserView {
        id: user.id,
        username: user.username,
        display_name: user.display_name,
        role: user.role,
        disabled: false,
        created_at: 0,
        last_login_at: None,
    }))
}

/// POST /v1/users/me/password — change the signed-in user's own password.
pub async fn change_own_password(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<ChangePasswordRequest>,
) -> Result<Json<ResolveResponse>> {
    let session = crate::ui_auth::request_user(&headers).ok_or_else(|| {
        AppError(
            StatusCode::BAD_REQUEST,
            "password changes require a browser session".into(),
        )
    })?;
    let new_hash = crate::ui_auth::hash_password(&req.new_password)
        .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?;
    let db = state.db.clone();
    let changed = spawn_blocking(move || -> anyhow::Result<bool> {
        let conn = db.lock().unwrap();
        let user =
            db::get_user(&conn, session.id)?.ok_or_else(|| anyhow::anyhow!("user not found"))?;
        if !crate::ui_auth::verify_password(&req.current_password, &user.password_hash) {
            anyhow::bail!("current password is incorrect");
        }
        db::set_user_password(&conn, user.id, &new_hash)
    })
    .await?
    .map_err(|e| AppError(StatusCode::BAD_REQUEST, format!("{e:#}")))?;
    Ok(Json(ResolveResponse { changed }))
}

// ── GET /v1/projects/:ident/messages/unread ───────────────────────────────────

#[derive(Serialize)]
pub struct GetUnreadResponse {
    pub messages: Vec<Message>,
    pub status: String,
}

pub async fn get_unread_messages(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path(ident): Path<String>,
) -> Result<Json<GetUnreadResponse>> {
    {
        let conn = state.db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            return Err(AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            ));
        }
    }

    let agent_id = extract_agent_id(&headers);
    let db = state.db.clone();
    let ident_clone = ident.clone();
    let aid = agent_id;
    let messages = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::upsert_agent(&conn, &ident_clone, &aid)?;
        db::get_unconfirmed_for_agent(&conn, &ident_clone, &aid)
    })
    .await??;

    let status = if messages.is_empty() {
        "no messages".to_string()
    } else {
        format!("{} unread user message(s)", messages.len())
    };

    Ok(Json(GetUnreadResponse { messages, status }))
}

// ── POST /v1/projects/:ident/messages/:id/confirm ────────────────────────────

#[derive(Serialize)]
pub struct ConfirmResponse {
    pub confirmed: bool,
}

pub async fn confirm_message(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((ident, msg_id)): Path<(String, i64)>,
) -> Result<Json<ConfirmResponse>> {
    {
        let conn = state.db.lock().unwrap();
        if db::get_project(&conn, &ident)?.is_none() {
            return Err(AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            ));
        }
    }

    let agent_id = extract_agent_id(&headers);
    let db = state.db.clone();
    let ident_clone = ident.clone();
    let aid = agent_id;
    let confirmed = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::confirm_message_for_agent(&conn, &ident_clone, &aid, msg_id)
    })
    .await??;

    Ok(Json(ConfirmResponse { confirmed }))
}

// ── POST /v1/projects/:ident/messages/:id/reply ─────────────────────────────

#[derive(Deserialize)]
pub struct ReplyRequest {
    /// Back-compat alias for `body`. If both are set, `body` wins.
    pub content: Option<String>,
    pub body: Option<String>,
    pub subject: Option<String>,
    pub hostname: Option<String>,
    pub event_at: Option<i64>,
}

#[derive(Serialize)]
pub struct ReplyResponse {
    pub message_id: i64,
    pub external_message_id: String,
    pub parent_message_id: i64,
}

pub async fn reply_to_message(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((ident, parent_id)): Path<(String, i64)>,
    Json(req): Json<ReplyRequest>,
) -> Result<Json<ReplyResponse>> {
    let agent_id = extract_agent_id(&headers);

    let (channel_name, room_id, parent_external_id) = {
        let conn = state.db.lock().unwrap();
        let project = db::get_project(&conn, &ident)?.ok_or_else(|| {
            AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            )
        })?;
        let parent = db::get_message_by_id(&conn, &ident, parent_id)?.ok_or_else(|| {
            AppError(
                StatusCode::NOT_FOUND,
                format!("message {} not found", parent_id),
            )
        })?;
        (
            project.channel_name,
            project.room_id,
            parent.external_message_id,
        )
    };

    let plugin = state
        .plugins
        .get(&channel_name)
        .ok_or_else(|| {
            AppError(
                StatusCode::SERVICE_UNAVAILABLE,
                format!("channel plugin '{channel_name}' is not configured"),
            )
        })?
        .clone();
    let room_id = ensure_project_room(&state, &ident, plugin.as_ref(), room_id).await?;

    let body_text = req.body.or(req.content).unwrap_or_default();
    if body_text.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "request must include non-empty 'body' (or 'content')".into(),
        ));
    }
    let outbound = build_outbound(
        &agent_id,
        body_text,
        req.subject,
        req.hostname,
        req.event_at,
    );

    let external_id = match &parent_external_id {
        Some(ext_id) => plugin.reply_structured(&room_id, ext_id, &outbound).await?,
        None => plugin.send_structured(&room_id, &outbound).await?,
    };

    let msg = Message {
        id: 0,
        project_ident: ident.clone(),
        source: "agent".into(),
        external_message_id: Some(external_id.clone()),
        content: outbound.body.clone(),
        sent_at: now_ms(),
        confirmed_at: None,
        parent_message_id: Some(parent_id),
        agent_id: Some(agent_id.clone()),
        message_type: "reply".into(),
        subject: Some(outbound.subject.clone()),
        hostname: Some(outbound.hostname.clone()),
        event_at: Some(outbound.event_at),
        deliver_to_agents: false,
        author_kind: Some("agent".to_string()),
        resolved_at: None,
        resolved_by: None,
    };

    let db = state.db.clone();
    let ident_clone = ident.clone();
    let aid = agent_id;
    let row_id = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::upsert_agent(&conn, &ident_clone, &aid)?;
        db::insert_message(&conn, &msg)
    })
    .await??;

    Ok(Json(ReplyResponse {
        message_id: row_id,
        external_message_id: external_id,
        parent_message_id: parent_id,
    }))
}

// ── POST /v1/projects/:ident/messages/:id/action ────────────────────────────

#[derive(Deserialize)]
pub struct ActionRequest {
    /// Back-compat alias for `body`. If both are set, `body` wins.
    pub message: Option<String>,
    pub body: Option<String>,
    pub subject: Option<String>,
    pub hostname: Option<String>,
    pub event_at: Option<i64>,
}

#[derive(Serialize)]
pub struct ActionResponse {
    pub message_id: i64,
    pub external_message_id: String,
    pub parent_message_id: i64,
}

pub async fn taking_action_on(
    State(state): State<AppState>,
    headers: HeaderMap,
    Path((ident, parent_id)): Path<(String, i64)>,
    Json(req): Json<ActionRequest>,
) -> Result<Json<ActionResponse>> {
    let agent_id = extract_agent_id(&headers);

    let (channel_name, room_id, parent_external_id) = {
        let conn = state.db.lock().unwrap();
        let project = db::get_project(&conn, &ident)?.ok_or_else(|| {
            AppError(
                StatusCode::NOT_FOUND,
                format!("project '{}' not found", ident),
            )
        })?;
        let parent = db::get_message_by_id(&conn, &ident, parent_id)?.ok_or_else(|| {
            AppError(
                StatusCode::NOT_FOUND,
                format!("message {} not found", parent_id),
            )
        })?;
        (
            project.channel_name,
            project.room_id,
            parent.external_message_id,
        )
    };

    let plugin = state
        .plugins
        .get(&channel_name)
        .ok_or_else(|| {
            AppError(
                StatusCode::SERVICE_UNAVAILABLE,
                format!("channel plugin '{channel_name}' is not configured"),
            )
        })?
        .clone();
    let room_id = ensure_project_room(&state, &ident, plugin.as_ref(), room_id).await?;

    let body_text = req.body.or(req.message).unwrap_or_default();
    if body_text.trim().is_empty() {
        return Err(AppError(
            StatusCode::BAD_REQUEST,
            "request must include non-empty 'body' (or 'message')".into(),
        ));
    }
    // Action posts get an `[ACTION]` subject prefix when the agent doesn't
    // supply one, so they remain visually distinct from regular replies.
    let subject = req.subject.or_else(|| {
        let derived = derive_subject(&body_text);
        Some(format!("[ACTION] {}", derived))
    });
    let outbound = build_outbound(&agent_id, body_text, subject, req.hostname, req.event_at);

    let external_id = match &parent_external_id {
        Some(ext_id) => plugin.reply_structured(&room_id, ext_id, &outbound).await?,
        None => plugin.send_structured(&room_id, &outbound).await?,
    };

    let msg = Message {
        id: 0,
        project_ident: ident.clone(),
        source: "agent".into(),
        external_message_id: Some(external_id.clone()),
        content: outbound.body.clone(),
        sent_at: now_ms(),
        confirmed_at: None,
        parent_message_id: Some(parent_id),
        agent_id: Some(agent_id.clone()),
        message_type: "action".into(),
        subject: Some(outbound.subject.clone()),
        hostname: Some(outbound.hostname.clone()),
        event_at: Some(outbound.event_at),
        deliver_to_agents: false,
        author_kind: Some("agent".to_string()),
        resolved_at: None,
        resolved_by: None,
    };

    let db = state.db.clone();
    let ident_clone = ident.clone();
    let aid = agent_id;
    let row_id = spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::upsert_agent(&conn, &ident_clone, &aid)?;
        db::insert_message(&conn, &msg)
    })
    .await??;

    Ok(Json(ActionResponse {
        message_id: row_id,
        external_message_id: external_id,
        parent_message_id: parent_id,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn actions_for_status_covers_transitions() {
        let todo: Vec<_> = actions_for_status("todo")
            .into_iter()
            .map(|a| (a.verb, a.target_status))
            .collect();
        assert_eq!(
            todo,
            vec![
                ("Claim".into(), "in_progress".into()),
                ("Done".into(), "done".into()),
                ("User review".into(), "user_review".into())
            ],
        );

        let in_progress: Vec<_> = actions_for_status("in_progress")
            .into_iter()
            .map(|a| (a.verb, a.target_status))
            .collect();
        assert_eq!(
            in_progress,
            vec![
                ("Release".into(), "todo".into()),
                ("Done".into(), "done".into()),
                ("User review".into(), "user_review".into())
            ],
        );

        let done: Vec<_> = actions_for_status("done")
            .into_iter()
            .map(|a| (a.verb, a.target_status))
            .collect();
        assert_eq!(done, vec![("Reopen".into(), "todo".into())]);

        let review: Vec<_> = actions_for_status("user_review")
            .into_iter()
            .map(|a| (a.verb, a.target_status))
            .collect();
        assert_eq!(
            review,
            vec![
                ("Return to TODO".into(), "todo".into()),
                ("Close".into(), "done".into())
            ]
        );
        assert!(actions_for_status("nonsense").is_empty());
    }

    fn native_task_headers() -> HeaderMap {
        let mut headers = HeaderMap::new();
        for (name, value) in [
            ("x-agent-session-id", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
            (
                "x-agent-instance-id",
                "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
            ),
            ("x-agent-provider", "codex"),
            ("x-agent-os", "linux"),
        ] {
            headers.insert(name, value.parse().unwrap());
        }
        headers
    }

    #[tokio::test]
    async fn task_goal_queues_are_explicit_and_preserve_legacy_tasks() {
        let state = test_state();
        let mut agent = native_task_headers();
        agent.insert("x-agent-id", "worker-a".parse().unwrap());
        let create = |value| Json(serde_json::from_value::<CreateTaskRequest>(value).unwrap());
        let Json(plain) = create_task_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            create(serde_json::json!({"title":"General work"})),
        )
        .await
        .unwrap();
        let Json(scoped) = create_task_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            create(serde_json::json!({"title":"Goal work", "goal":"  release  "})),
        )
        .await
        .unwrap();
        assert_eq!(scoped.task.goal.as_deref(), Some("release"));
        let query = |value| Query(serde_json::from_value::<ListTasksQuery>(value).unwrap());
        let Json(default) = list_tasks_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            query(serde_json::json!({})),
        )
        .await
        .unwrap();
        assert_eq!(
            default.iter().map(|t| t.id.as_str()).collect::<Vec<_>>(),
            vec![plain.task.id.as_str()]
        );
        let Json(human) = list_tasks_handler(
            State(state.clone()),
            HeaderMap::new(),
            Path("demo".into()),
            query(serde_json::json!({})),
        )
        .await
        .unwrap();
        assert_eq!(human.len(), 2);
        let Json(all) = list_tasks_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            query(serde_json::json!({"all_goals":true})),
        )
        .await
        .unwrap();
        assert_eq!(all.len(), 2);
        let Json(selected) = list_tasks_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            query(serde_json::json!({"goal":"release"})),
        )
        .await
        .unwrap();
        assert_eq!(selected.len(), 1);
        assert_eq!(selected[0].id, scoped.task.id);
        assert_eq!(selected[0].goal.as_deref(), Some("release"));
        agent.insert("x-task-goal", "release".parse().unwrap());
        let Json(inherited) = create_task_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            create(serde_json::json!({"title":"Inherit context"})),
        )
        .await
        .unwrap();
        assert_eq!(inherited.task.goal.as_deref(), Some("release"));
        let Json(ungrouped) = list_tasks_handler(
            State(state.clone()),
            agent,
            Path("demo".into()),
            query(serde_json::json!({"goal":""})),
        )
        .await
        .unwrap();
        assert_eq!(ungrouped.len(), 1);
        let conn = state.db.lock().unwrap();
        let nudges: i64 = conn
            .query_row(
                "SELECT count(*) FROM messages WHERE subject='Task created'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        assert_eq!(nudges, 0, "goal work must not create project-wide nudges");
        let events = crate::task_events::list(&conn, &Default::default()).unwrap();
        let event = events
            .iter()
            .find(|e| e.task["id"] == scoped.task.id)
            .unwrap();
        assert_eq!(
            event.task["goal"], "release",
            "the initial event must include the goal"
        );
    }

    #[tokio::test]
    async fn human_resume_requeues_ordinary_tasks_and_approves_findings_atomically() {
        for finding in [false, true] {
            for via_reorder in [false, true] {
                let state = test_state();
                let agent = native_task_headers();
                let Json(created) = create_task_handler(
                State(state.clone()), agent.clone(), Path("demo".into()),
                Json(serde_json::from_value(serde_json::json!({
                    "title": "Review task", "labels": if finding { vec!["autoreview-finding", "os:linux", "bug"] } else { vec![] }
                })).unwrap()),
            ).await.unwrap();
                let id = created.task.id;
                let path = || Path(("demo".into(), id.clone()));
                let update =
                    |value| Json(serde_json::from_value::<UpdateTaskRequest>(value).unwrap());
                let _ = update_task_handler(
                    State(state.clone()),
                    agent.clone(),
                    path(),
                    update(serde_json::json!({"status":"user_review"})),
                )
                .await
                .unwrap();
                // Latest unrelated labels and a prior exact-session route live in the DB,
                // not in a stale desktop GET. Resume must preserve the former, clear the latter.
                {
                    let conn = state.db.lock().unwrap();
                    conn.execute("UPDATE tasks SET labels=?1, routing_json=?2 WHERE id=?3", rusqlite::params![
                    serde_json::json!(if finding { vec!["autoreview-finding", "os:linux", "bug", "latest-label"] } else { vec![] }).to_string(),
                    serde_json::json!({"mode":"session", "target":{
                        "origin": crate::task_events::origin(&agent).unwrap().unwrap(), "generation":"old-generation"
                    }}).to_string(), id,
                ]).unwrap();
                }
                let _ = add_comment_handler(
                    State(state.clone()),
                    HeaderMap::new(),
                    path(),
                    Json(AddCommentRequest {
                        content: "Please continue after this note".into(),
                        hostname: None,
                        author: None,
                        author_type: None,
                    }),
                )
                .await
                .unwrap();
                let Json(before) =
                    get_task_handler(State(state.clone()), path(), Query(GetTaskQuery::default()))
                        .await
                        .unwrap();
                assert_eq!(before.task.status, "user_review");
                assert_eq!(
                    before.task.labels.iter().any(|l| l == "autoreview-finding"),
                    finding
                );
                let mut legacy_agent = HeaderMap::new();
                legacy_agent.insert("x-agent-id", "agent-only-header".parse().unwrap());
                for headers in [agent.clone(), legacy_agent] {
                    for request in [
                        serde_json::json!({"status":"todo"}),
                        serde_json::json!({"labels":["os:linux", "bug"]}),
                    ] {
                        assert_eq!(
                            update_task_handler(
                                State(state.clone()),
                                headers.clone(),
                                path(),
                                update(request)
                            )
                            .await
                            .unwrap_err()
                            .0,
                            StatusCode::CONFLICT
                        );
                    }
                }
                if via_reorder {
                    // The whole batch, including label removal, rolls back on a stale/missing row.
                    assert!(reorder_tasks_handler(
                        State(state.clone()),
                        HeaderMap::new(),
                        Path("demo".into()),
                        Query(ReorderTasksQuery {
                            goal: None,
                            status: "todo".into()
                        }),
                        Json(ReorderTasksRequest {
                            order: vec![id.clone(), "missing".into()]
                        })
                    )
                    .await
                    .is_err());
                    let Json(rolled_back) = get_task_handler(
                        State(state.clone()),
                        path(),
                        Query(GetTaskQuery::default()),
                    )
                    .await
                    .unwrap();
                    assert_eq!(rolled_back.task.status, "user_review");
                    assert_eq!(rolled_back.task.labels, before.task.labels);
                    assert_eq!(
                        serde_json::to_value(rolled_back.task.routing).unwrap(),
                        serde_json::to_value(&before.task.routing).unwrap()
                    );
                    let _ = reorder_tasks_handler(
                        State(state.clone()),
                        HeaderMap::new(),
                        Path("demo".into()),
                        Query(ReorderTasksQuery {
                            goal: None,
                            status: "todo".into(),
                        }),
                        Json(ReorderTasksRequest {
                            order: vec![id.clone()],
                        }),
                    )
                    .await
                    .unwrap();
                } else {
                    let _ = update_task_handler(
                        State(state.clone()),
                        HeaderMap::new(),
                        path(),
                        update(serde_json::json!({"status":"todo", "owner_agent_id":null})),
                    )
                    .await
                    .unwrap();
                }
                let Json(resumed) =
                    get_task_handler(State(state.clone()), path(), Query(GetTaskQuery::default()))
                        .await
                        .unwrap();
                assert_eq!(resumed.task.status, "todo");
                assert_eq!(
                    resumed.task.labels,
                    if finding {
                        vec!["os:linux", "bug", "latest-label"]
                    } else {
                        vec![]
                    }
                );
                assert!(
                    resumed.task.owner_agent_id.is_none() && resumed.task.owner_origin.is_none()
                );
                assert!(resumed.task.started_at.is_none() && resumed.task.done_at.is_none());
                assert_eq!(
                    serde_json::to_value(&resumed.task.routing).unwrap(),
                    serde_json::json!({"mode":"auto"})
                );
                // OS requirements still gate the next compatible claimant.
                let mut windows = agent.clone();
                windows.insert("x-agent-os", "windows".parse().unwrap());
                if finding {
                    assert_eq!(
                        update_task_handler(
                            State(state.clone()),
                            windows,
                            path(),
                            update(serde_json::json!({"status":"in_progress"}))
                        )
                        .await
                        .unwrap_err()
                        .0,
                        StatusCode::CONFLICT
                    );
                }
                let _ = update_task_handler(
                    State(state.clone()),
                    agent.clone(),
                    path(),
                    update(serde_json::json!({"status":"in_progress"})),
                )
                .await
                .unwrap();
            }
        }
        // Human comments, same-column TODO updates and ordinary creation cannot
        // turn an untouched automatic finding into a triaged finding.
        let state = test_state();
        let Json(created) = create_task_handler(
            State(state.clone()),
            HeaderMap::new(),
            Path("demo".into()),
            Json(
                serde_json::from_value(
                    serde_json::json!({"title":"Untouched", "labels":["autoreview-finding"]}),
                )
                .unwrap(),
            ),
        )
        .await
        .unwrap();
        let path = || Path(("demo".into(), created.task.id.clone()));
        let Json(unchanged) = update_task_handler(
            State(state.clone()),
            HeaderMap::new(),
            path(),
            Json(serde_json::from_value(serde_json::json!({"status":"todo"})).unwrap()),
        )
        .await
        .unwrap();
        assert_eq!(unchanged.labels, vec!["autoreview-finding"]);
    }

    #[tokio::test]
    async fn user_review_holds_work_until_a_user_decides() {
        let state = test_state();
        let agent = native_task_headers();
        let Json(created) = create_task_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            Json(
                serde_json::from_value(serde_json::json!({
                    "title": "Still needed?", "specification": "Keep this context"
                }))
                .unwrap(),
            ),
        )
        .await
        .unwrap();
        let id = created.task.id;
        let path = || Path(("demo".into(), id.clone()));
        let update = |status: &str| {
            Json(
                serde_json::from_value::<UpdateTaskRequest>(serde_json::json!({"status": status}))
                    .unwrap(),
            )
        };
        let reorder = |status: &str| {
            Query(ReorderTasksQuery {
                goal: None,
                status: status.into(),
            })
        };
        for via_reorder in [false, true] {
            let _ = update_task_handler(
                State(state.clone()),
                agent.clone(),
                path(),
                update("in_progress"),
            )
            .await
            .unwrap();
            // A peer cannot move another session's claimed task to user review.
            let mut peer = agent.clone();
            peer.insert(
                "x-agent-session-id",
                "cccccccc-cccc-4ccc-8ccc-cccccccccccc".parse().unwrap(),
            );
            assert_eq!(
                update_task_handler(State(state.clone()), peer, path(), update("user_review"))
                    .await
                    .unwrap_err()
                    .0,
                StatusCode::CONFLICT
            );
            if via_reorder {
                let _ = reorder_tasks_handler(
                    State(state.clone()),
                    agent.clone(),
                    Path("demo".into()),
                    reorder("user_review"),
                    Json(ReorderTasksRequest {
                        order: vec![id.clone()],
                    }),
                )
                .await
                .unwrap();
            } else {
                let _ = update_task_handler(
                    State(state.clone()),
                    agent.clone(),
                    path(),
                    update("user_review"),
                )
                .await
                .unwrap();
            }
            let Json(detail) =
                get_task_handler(State(state.clone()), path(), Query(GetTaskQuery::default()))
                    .await
                    .unwrap();
            assert_eq!(detail.task.status, "user_review");
            assert!(detail.task.owner_agent_id.is_none());
            assert!(detail.task.owner_origin.is_none());
            assert!(detail.task.started_at.is_none());
            assert!(detail.task.done_at.is_none());
            assert_eq!(detail.task.details.as_deref(), Some("Keep this context"));
            let Json(review) = list_tasks_handler(
                State(state.clone()),
                HeaderMap::new(),
                Path("demo".into()),
                Query(
                    serde_json::from_value(serde_json::json!({
                        "status": "user_review"
                    }))
                    .unwrap(),
                ),
            )
            .await
            .unwrap();
            assert_eq!(review.len(), 1);
            let Json(queue) = list_tasks_handler(
                State(state.clone()),
                agent.clone(),
                Path("demo".into()),
                Query(serde_json::from_value(serde_json::json!({})).unwrap()),
            )
            .await
            .unwrap();
            assert!(queue.is_empty());
            for status in ["todo", "in_progress", "done"] {
                assert_eq!(
                    update_task_handler(
                        State(state.clone()),
                        agent.clone(),
                        path(),
                        update(status)
                    )
                    .await
                    .unwrap_err()
                    .0,
                    StatusCode::CONFLICT
                );
                assert_eq!(
                    reorder_tasks_handler(
                        State(state.clone()),
                        agent.clone(),
                        Path("demo".into()),
                        reorder(status),
                        Json(ReorderTasksRequest {
                            order: vec![id.clone()]
                        })
                    )
                    .await
                    .unwrap_err()
                    .0,
                    StatusCode::CONFLICT
                );
            }
            {
                let conn = state.db.lock().unwrap();
                conn.execute("UPDATE tasks SET updated_at=0 WHERE id=?1", [&id])
                    .unwrap();
                assert_eq!(db::reclaim_stale_tasks(&conn, "demo").unwrap(), 0);
            }
            // Browser decisions work after ownership has been released.
            if via_reorder {
                let _ = reorder_tasks_handler(
                    State(state.clone()),
                    HeaderMap::new(),
                    Path("demo".into()),
                    reorder("todo"),
                    Json(ReorderTasksRequest {
                        order: vec![id.clone()],
                    }),
                )
                .await
                .unwrap();
            } else {
                let _ = update_task_handler(
                    State(state.clone()),
                    HeaderMap::new(),
                    path(),
                    update("todo"),
                )
                .await
                .unwrap();
            }
        }
        let _ = update_task_handler(
            State(state.clone()),
            agent.clone(),
            path(),
            update("user_review"),
        )
        .await
        .unwrap();
        let Json(closed) = update_task_handler(
            State(state.clone()),
            HeaderMap::new(),
            path(),
            update("done"),
        )
        .await
        .unwrap();
        assert_eq!(closed.status, "done");
        assert!(closed.done_at.is_some());
    }

    #[tokio::test]
    async fn task_goal_claims_require_matching_context_and_can_be_cleared() {
        let state = test_state();
        let Json(created) = create_task_handler(
            State(state.clone()),
            HeaderMap::new(),
            Path("demo".into()),
            Json(
                serde_json::from_value(serde_json::json!({"title":"Scoped", "goal":"release"}))
                    .unwrap(),
            ),
        )
        .await
        .unwrap();
        let path = || Path(("demo".into(), created.task.id.clone()));
        let update = |value| Json(serde_json::from_value::<UpdateTaskRequest>(value).unwrap());
        let mut agent = native_task_headers();
        agent.insert("x-agent-id", "worker-b".parse().unwrap());
        // Goal mutations of a claim require the exact claiming session too.
        agent.insert(
            "x-agent-session-id",
            "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa".parse().unwrap(),
        );
        agent.insert(
            "x-agent-instance-id",
            "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb".parse().unwrap(),
        );
        agent.insert("x-agent-provider", "codex".parse().unwrap());
        agent.insert("x-agent-os", "linux".parse().unwrap());
        for goal in [None, Some("different")] {
            if let Some(goal) = goal {
                agent.insert("x-task-goal", goal.parse().unwrap());
            }
            let error = update_task_handler(
                State(state.clone()),
                agent.clone(),
                path(),
                update(serde_json::json!({"status":"in_progress"})),
            )
            .await
            .unwrap_err();
            assert_eq!(error.0, StatusCode::CONFLICT);
        }
        // Reorder is also a claim path and cannot bypass goal selection.
        let error = reorder_tasks_handler(
            State(state.clone()),
            agent.clone(),
            Path("demo".into()),
            Query(ReorderTasksQuery {
                goal: None,
                status: "in_progress".into(),
            }),
            Json(ReorderTasksRequest {
                order: vec![created.task.id.clone()],
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(error.0, StatusCode::CONFLICT);
        agent.insert("x-task-goal", "release".parse().unwrap());
        let Json(claimed) = update_task_handler(
            State(state.clone()),
            agent.clone(),
            path(),
            update(serde_json::json!({"status":"in_progress"})),
        )
        .await
        .unwrap();
        assert_eq!(claimed.owner_agent_id.as_deref(), Some("worker-b"));
        let Json(retained) = update_task_handler(
            State(state.clone()),
            agent.clone(),
            path(),
            update(serde_json::json!({"title":"Renamed"})),
        )
        .await
        .unwrap();
        assert_eq!(retained.goal.as_deref(), Some("release"));
        let Json(cleared) = update_task_handler(
            State(state.clone()),
            agent,
            path(),
            update(serde_json::json!({"goal":null})),
        )
        .await
        .unwrap();
        assert_eq!(cleared.goal, None);
        let error = update_task_handler(
            State(state),
            HeaderMap::new(),
            path(),
            update(serde_json::json!({"goal":123})),
        )
        .await
        .unwrap_err();
        assert_eq!(error.0, StatusCode::BAD_REQUEST);
    }

    fn sample_task() -> db::Task {
        db::Task {
            id: "task-1".to_string(),
            project_ident: "demo-project".to_string(),
            title: "Demo task".to_string(),
            goal: None,
            description: Some("Short context".to_string()),
            details: Some("Long handoff spec".to_string()),
            status: "todo".to_string(),
            rank: 1,
            labels: vec!["demo".to_string()],
            hostname: Some("host".to_string()),
            owner_agent_id: None,
            owner_origin: None,
            routing: Default::default(),
            requester: None,
            reporter: "agent".to_string(),
            created_at: 1,
            updated_at: 1,
            started_at: None,
            done_at: None,
            kind: "normal".to_string(),
            delegated_to_project_ident: None,
            delegated_to_task_id: None,
        }
    }

    #[test]
    fn task_create_response_includes_specification_alias_and_hint() {
        let value = serde_json::to_value(TaskCreateResponse::new(sample_task())).unwrap();

        assert_eq!(value["details"], "Long handoff spec");
        assert_eq!(value["specification"], "Long handoff spec");
        assert_eq!(value["hint"], TASK_SPECIFICATION_HINT);
    }

    #[test]
    fn task_detail_response_includes_specification_alias() {
        let value =
            serde_json::to_value(TaskWithComments::new(sample_task(), Vec::new(), 0)).unwrap();

        assert_eq!(value["details"], "Long handoff spec");
        assert_eq!(value["specification"], "Long handoff spec");
        assert!(value["actions"].is_array());
    }

    #[tokio::test]
    async fn register_project_without_remote_is_adhoc_and_creates_no_room() {
        let state = test_state();
        let Json(resp) = register_project(
            State(state.clone()),
            Json(RegisterProjectRequest {
                ident: "/home/someone/tmp".into(),
                channel: None,
                repo_url: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(resp.ident, "tmp");
        assert_eq!(resp.kind, db::PROJECT_KIND_ADHOC);
        assert_eq!(resp.room_id, "");
        assert!(resp.repo_full_name.is_none());

        // Re-registering with the remote heals the mapping without a plugin.
        let Json(healed) = register_project(
            State(state),
            Json(RegisterProjectRequest {
                ident: "tmp".into(),
                channel: None,
                repo_url: Some("git@github.com:nitecon/tmp.git".into()),
            }),
        )
        .await
        .unwrap();
        assert_eq!(healed.kind, db::PROJECT_KIND_REPO);
        assert_eq!(healed.repo_full_name.as_deref(), Some("nitecon/tmp"));
    }

    #[tokio::test]
    async fn archive_and_restore_toggle_default_listing() {
        let state = test_state();
        let Json(archived) = archive_project(State(state.clone()), Path("demo".into()))
            .await
            .unwrap();
        assert!(archived.archived_at.is_some());
        {
            let conn = state.db.lock().unwrap();
            assert!(db::list_project_stats(&conn).unwrap().is_empty());
            assert_eq!(
                db::list_project_stats_filtered(&conn, true).unwrap().len(),
                1
            );
        }
        let Json(restored) = restore_project(State(state.clone()), Path("demo".into()))
            .await
            .unwrap();
        assert!(restored.archived_at.is_none());
        let conn = state.db.lock().unwrap();
        assert_eq!(db::list_project_stats(&conn).unwrap().len(), 1);
    }

    #[tokio::test]
    async fn human_message_is_stored_even_when_channel_is_unavailable() {
        let state = test_state();
        let Json(first) = post_human_message(
            State(state.clone()),
            HeaderMap::new(),
            Path("demo".into()),
            Json(HumanMessageRequest {
                content: "Please look at the widget".into(),
                parent_message_id: None,
                author: Some("will".into()),
                resolve: None,
            }),
        )
        .await
        .unwrap();
        assert!(!first.delivered);
        assert!(first.delivery_error.is_some());
        assert!(first.parent_message_id.is_none());

        // Reply into the thread using the form-shaped payload and resolve it.
        let Json(reply) = post_human_message(
            State(state.clone()),
            HeaderMap::new(),
            Path("demo".into()),
            Json(HumanMessageRequest {
                content: "Never mind, done.".into(),
                parent_message_id: Some(Value::String(first.message_id.to_string())),
                author: None,
                resolve: Some(Value::String("on".into())),
            }),
        )
        .await
        .unwrap();
        assert_eq!(reply.parent_message_id, Some(first.message_id));

        let Json(list) = list_message_threads(
            State(state.clone()),
            Path("demo".into()),
            Query(ListThreadsQuery {
                state: Some("all".into()),
                ..Default::default()
            }),
        )
        .await
        .unwrap();
        assert_eq!(list.threads.len(), 1);
        assert_eq!(list.threads[0].state, "resolved");
        assert_eq!(list.threads[0].reply_count, 1);
        assert_eq!(list.threads[0].author_kind, "human");

        // Agents see the human message in their unread queue.
        {
            let conn = state.db.lock().unwrap();
            let unread = db::get_unconfirmed_for_agent(&conn, "demo", "agent-x").unwrap();
            assert_eq!(unread.len(), 2);
        }

        // Empty content is rejected.
        let err = post_human_message(
            State(state),
            HeaderMap::new(),
            Path("demo".into()),
            Json(HumanMessageRequest {
                content: "   ".into(),
                parent_message_id: None,
                author: None,
                resolve: None,
            }),
        )
        .await
        .err()
        .unwrap();
        assert_eq!(err.0, StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn users_api_enforces_admin_and_protects_last_admin() {
        let state = test_state();
        // Bearer callers (no session headers) act as administrators.
        let Json(admin) = create_user(
            State(state.clone()),
            HeaderMap::new(),
            Json(CreateUserRequest {
                username: "will".into(),
                password: "correct horse".into(),
                display_name: None,
                role: Some("admin".into()),
            }),
        )
        .await
        .unwrap();
        let Json(member) = create_user(
            State(state.clone()),
            HeaderMap::new(),
            Json(CreateUserRequest {
                username: "ann".into(),
                password: "correct horse".into(),
                display_name: Some("Ann".into()),
                role: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(member.role, "member");

        // A member session may not manage users.
        let mut member_headers = HeaderMap::new();
        member_headers.insert(
            crate::ui_auth::USER_HEADER,
            HeaderValue::from_str(&format!("{}:ann:Ann", member.id)).unwrap(),
        );
        member_headers.insert(
            crate::ui_auth::USER_ROLE_HEADER,
            HeaderValue::from_static("member"),
        );
        let err = list_users(State(state.clone()), member_headers.clone())
            .await
            .err()
            .unwrap();
        assert_eq!(err.0, StatusCode::FORBIDDEN);

        // ...but may change their own password with the current one.
        let Json(changed) = change_own_password(
            State(state.clone()),
            member_headers.clone(),
            Json(ChangePasswordRequest {
                current_password: "correct horse".into(),
                new_password: "battery staple".into(),
            }),
        )
        .await
        .unwrap();
        assert!(changed.changed);
        let wrong = change_own_password(
            State(state.clone()),
            member_headers,
            Json(ChangePasswordRequest {
                current_password: "correct horse".into(),
                new_password: "another one!".into(),
            }),
        )
        .await
        .err()
        .unwrap();
        assert_eq!(wrong.0, StatusCode::BAD_REQUEST);

        // The last active admin cannot be demoted, disabled, or deleted.
        let demote = update_user(
            State(state.clone()),
            HeaderMap::new(),
            Path(admin.id),
            Json(UpdateUserRequest {
                role: Some("member".into()),
                ..Default::default()
            }),
        )
        .await
        .err()
        .unwrap();
        assert_eq!(demote.0, StatusCode::BAD_REQUEST);
        let del = delete_user(State(state.clone()), HeaderMap::new(), Path(admin.id))
            .await
            .err()
            .unwrap();
        assert_eq!(del.0, StatusCode::BAD_REQUEST);

        // Members can be disabled and deleted.
        let Json(disabled) = update_user(
            State(state.clone()),
            HeaderMap::new(),
            Path(member.id),
            Json(UpdateUserRequest {
                disabled: Some(Value::String("true".into())),
                ..Default::default()
            }),
        )
        .await
        .unwrap();
        assert!(disabled.disabled);
        let Json(removed) = delete_user(State(state.clone()), HeaderMap::new(), Path(member.id))
            .await
            .unwrap();
        assert!(removed.changed);
        let Json(users) = list_users(State(state), HeaderMap::new()).await.unwrap();
        assert_eq!(users.len(), 1);
    }

    #[tokio::test]
    async fn browser_actions_are_attributed_to_the_session_user() {
        let state = test_state();
        let mut headers = HeaderMap::new();
        headers.insert(
            crate::ui_auth::USER_HEADER,
            HeaderValue::from_static("3:will:Will H"),
        );
        headers.insert(
            crate::ui_auth::USER_ROLE_HEADER,
            HeaderValue::from_static("admin"),
        );
        let Json(first) = post_human_message(
            State(state.clone()),
            headers.clone(),
            Path("demo".into()),
            Json(HumanMessageRequest {
                content: "hello agents".into(),
                parent_message_id: None,
                author: None,
                resolve: None,
            }),
        )
        .await
        .unwrap();
        assert!(first.delivery_error.is_some());
        let Json(resolved) = resolve_message(
            State(state.clone()),
            headers,
            Path(("demo".into(), first.message_id)),
        )
        .await
        .unwrap();
        assert!(resolved.changed);
        let conn = state.db.lock().unwrap();
        let stored = db::get_message_by_id(&conn, "demo", first.message_id)
            .unwrap()
            .unwrap();
        assert_eq!(stored.resolved_by.as_deref(), Some("user:will"));
    }

    fn test_state_with_operations(artifact_operations: db::ArtifactOperationsEnvelope) -> AppState {
        let db = db::open(":memory:").unwrap();
        {
            let conn = db.lock().unwrap();
            db::insert_project(
                &conn,
                &db::Project {
                    ident: "demo".to_string(),
                    channel_name: "discord".to_string(),
                    room_id: "room-demo".to_string(),
                    last_msg_id: None,
                    created_at: db::now_ms(),
                    repo_provider: None,
                    repo_namespace: None,
                    repo_name: None,
                    repo_full_name: None,
                    kind: db::PROJECT_KIND_REPO.to_string(),
                    archived_at: None,
                    canonical_remote: None,
                },
            )
            .unwrap();
        }
        AppState {
            db,
            plugins: std::sync::Arc::new(std::collections::HashMap::new()),
            #[cfg(feature = "whatsapp")]
            whatsapp: None,
            default_channel: "discord".to_string(),
            api_key: "test-key".to_string(),
            ui_auth_enabled: true,
            retention_days: 30,
            bot_retention_days: 7,
            artifact_operations,
            artifact_body_schema_enabled: true,
            artifact_auth_enforced: false,
            remote: Default::default(),
        }
    }

    fn test_state() -> AppState {
        test_state_with_operations(db::ArtifactOperationsEnvelope::production_defaults())
    }

    #[tokio::test]
    async fn task_reads_never_reclaim_work_and_clock_controller_owns_expiry() {
        let state = test_state();
        let id = "01a17e58-0000-7000-8000-000000000001";
        {
            let conn = state.db.lock().unwrap();
            conn.execute("INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at,status,owner_agent_id,started_at)
                VALUES(?1,'demo','stale work','user',0,0,'in_progress','owner',0)", [id]).unwrap();
        }
        let Json(tasks) = list_tasks_handler(
            State(state.clone()),
            HeaderMap::new(),
            Path("demo".into()),
            Query(serde_json::from_value(serde_json::json!({})).unwrap()),
        )
        .await
        .unwrap();
        assert_eq!(tasks.len(), 1);
        assert_eq!(tasks[0].status, "in_progress");
        let Json(detail) = get_task_handler(
            State(state.clone()),
            Path(("demo".into(), id.into())),
            Query(GetTaskQuery::default()),
        )
        .await
        .unwrap();
        assert_eq!(detail.task.status, "in_progress");
        assert_eq!(detail.task.owner_agent_id.as_deref(), Some("owner"));
        assert!(detail.comments.is_empty());

        let (bus, worker) = crate::router::start(
            state.db.clone(),
            crate::plugins::events(),
            vec![
                Box::new(crate::plugins::presence::Presence),
                Box::new(crate::plugins::reclaim::Reclaim),
            ],
        )
        .unwrap();
        bus.emit(
            "test",
            crate::plugins::clock::TICK,
            1,
            serde_json::json!({}),
            None,
        )
        .await
        .unwrap();
        let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(5);
        loop {
            let detail = db::get_task_detail(&state.db.lock().unwrap(), "demo", id)
                .unwrap()
                .unwrap();
            if detail.task.status == "todo" {
                assert!(detail.task.owner_agent_id.is_none());
                assert_eq!(detail.comments.len(), 1);
                break;
            }
            assert!(
                tokio::time::Instant::now() < deadline,
                "Reclaim did not reconcile expiry"
            );
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        worker.abort();
    }

    fn hardened_test_state() -> AppState {
        let mut state = test_state();
        state.artifact_auth_enforced = true;
        state
    }

    fn t008_operations_fixture() -> db::ArtifactOperationsEnvelope {
        db::t008_shrunken_artifact_operations_fixture()
    }

    fn mutation_headers(key: &str) -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert("x-agent-id", HeaderValue::from_static("codex-test"));
        headers.insert("x-agent-system", HeaderValue::from_static("codex"));
        headers.insert("idempotency-key", HeaderValue::from_str(key).unwrap());
        headers
    }

    fn authorized_headers(key: &str, scopes: &str) -> HeaderMap {
        let mut headers = mutation_headers(key);
        headers.insert("x-agent-project", HeaderValue::from_static("demo"));
        headers.insert("x-agent-scopes", HeaderValue::from_str(scopes).unwrap());
        headers
    }

    fn read_headers(scopes: &str) -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert("x-agent-project", HeaderValue::from_static("demo"));
        headers.insert("x-agent-scopes", HeaderValue::from_str(scopes).unwrap());
        headers
    }

    async fn response_json(response: Response) -> (StatusCode, Value) {
        let status = response.status();
        let bytes = axum::body::to_bytes(response.into_body(), usize::MAX)
            .await
            .unwrap();
        let value = serde_json::from_slice(&bytes).unwrap();
        (status, value)
    }

    #[allow(clippy::too_many_arguments)]
    async fn create_test_artifact(state: &AppState, key: &str) -> String {
        let response = create_artifact_handler(
            State(state.clone()),
            Path("demo".to_string()),
            mutation_headers(key),
            Json(CreateArtifactRequest {
                kind: "spec".to_string(),
                subkind: Some("implementation".to_string()),
                title: "Test spec".to_string(),
                labels: Some(vec!["test".to_string()]),
                actor_display_name: Some("Codex Test".to_string()),
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        assert_eq!(
            value["provenance"]["authorization"]["boundary"],
            "trusted-single-tenant"
        );
        value["data"]["artifact_id"].as_str().unwrap().to_string()
    }

    async fn create_test_version(
        state: &AppState,
        artifact_id: &str,
        key: &str,
        workflow_run_id: Option<&str>,
        parent_version_id: Option<&str>,
        body: &str,
    ) -> (StatusCode, Value) {
        let mut headers = mutation_headers(key);
        if let Some(run_id) = workflow_run_id {
            headers.insert("x-workflow-run-id", HeaderValue::from_str(run_id).unwrap());
        }
        let response = create_artifact_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.to_string())),
            headers,
            Json(CreateArtifactVersionRequest {
                version_label: Some(key.to_string()),
                parent_version_id: parent_version_id.map(str::to_string),
                body_format: Some("markdown".to_string()),
                body: Some(body.to_string()),
                structured_payload: None,
                source_format: None,
                version_state: Some("draft".to_string()),
                actor_display_name: Some("Codex Test".to_string()),
            }),
        )
        .await
        .unwrap();
        response_json(response).await
    }

    async fn start_test_run(state: &AppState, artifact_id: &str, key: &str) -> String {
        let response = start_artifact_workflow_run_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.to_string())),
            mutation_headers(key),
            Json(StartWorkflowRunRequest {
                workflow_kind: "spec_iteration".to_string(),
                phase: Some("test".to_string()),
                round_id: None,
                participant_actor_ids: None,
                source_artifact_version_id: None,
                read_set: None,
                is_resumable: Some(false),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        value["data"]["workflow_run_id"]
            .as_str()
            .unwrap()
            .to_string()
    }

    #[tokio::test]
    async fn memory_push_unknown_project_returns_rejected_batch_not_transport_404() {
        let state = test_state();
        let mut headers = HeaderMap::new();
        headers.insert(
            "x-memory-client-id",
            HeaderValue::from_static("memory-client"),
        );

        let Json(body) = push_memories_handler(
            State(state.clone()),
            headers,
            Path("missing-proj".to_string()),
            Json(PushMemoriesRequest {
                memories: vec![db::MemoryPushItem {
                    local_memory_id: Some("local-1".to_string()),
                    content: Some("project memory".to_string()),
                    memory_type: Some("project".to_string()),
                    ..db::MemoryPushItem::default()
                }],
            }),
        )
        .await
        .expect("unknown project pushes should be item rejections, not transport errors");

        assert_eq!(body.project_ident, "missing-proj");
        assert_eq!(body.server_revision, 0);
        assert_eq!(body.results.len(), 1);
        let result = &body.results[0];
        assert_eq!(result.action, "rejected");
        assert_eq!(result.local_memory_id.as_deref(), Some("local-1"));
        assert_eq!(result.client_id.as_deref(), Some("memory-client"));
        assert_eq!(
            result.error.as_deref(),
            Some("project 'missing-proj' not found on gateway")
        );

        let conn = state.db.lock().unwrap();
        assert!(db::get_project(&conn, "missing-proj").unwrap().is_none());
        assert_eq!(
            db::memory_project_revision(&conn, "missing-proj").unwrap(),
            0
        );
    }

    #[tokio::test]
    async fn design_review_routes_cover_two_pass_query_and_synthesis_version() {
        let state = test_state();
        let create_response = create_design_review_handler(
            State(state.clone()),
            Path("demo".to_string()),
            mutation_headers("review-create"),
            Json(DesignReviewCreateRequest {
                title: "Gateway review".to_string(),
                labels: Some(vec!["review".to_string()]),
                body: Some("# Design".to_string()),
                body_format: Some("markdown".to_string()),
                source_artifact_id: None,
                source_artifact_version_id: None,
                actor_display_name: Some("Codex Test".to_string()),
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(create_response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        let artifact_id = value["data"]["artifact"]["artifact_id"]
            .as_str()
            .unwrap()
            .to_string();
        let source_version_id = value["data"]["version"]["artifact_version_id"]
            .as_str()
            .unwrap()
            .to_string();

        let round_response = create_design_review_round_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("review-round"),
            Json(DesignReviewRoundRequest {
                round_id: Some("round-a".to_string()),
                participant_actor_ids: None,
                source_artifact_version_id: source_version_id.clone(),
                read_set: None,
                actor_display_name: Some("Codex Test".to_string()),
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(round_response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        let workflow_run_id = value["data"]["workflow_run"]["workflow_run_id"]
            .as_str()
            .unwrap()
            .to_string();
        assert_eq!(
            value["data"]["artifact"]["review_state"],
            "collecting_reviews"
        );

        let pass1_response = create_design_review_contribution_handler(
            State(state.clone()),
            Path((
                "demo".to_string(),
                artifact_id.clone(),
                workflow_run_id.clone(),
            )),
            mutation_headers("review-pass1"),
            Json(DesignReviewContributionRequest {
                phase: "pass_1".to_string(),
                role: None,
                reviewed_version_id: None,
                read_set: None,
                body_format: None,
                body: "pass one".to_string(),
                actor_display_name: Some("Codex Test".to_string()),
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(pass1_response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        let pass1_id = value["data"]["contribution_id"]
            .as_str()
            .unwrap()
            .to_string();

        let pass2_read_set = serde_json::json!({
            "versions": [source_version_id],
            "contributions": [pass1_id.clone()],
        });
        let pass2_response = create_design_review_contribution_handler(
            State(state.clone()),
            Path((
                "demo".to_string(),
                artifact_id.clone(),
                workflow_run_id.clone(),
            )),
            mutation_headers("review-pass2"),
            Json(DesignReviewContributionRequest {
                phase: "pass_2".to_string(),
                role: None,
                reviewed_version_id: None,
                read_set: Some(pass2_read_set.clone()),
                body_format: None,
                body: "pass two".to_string(),
                actor_display_name: Some("Codex Test".to_string()),
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(pass2_response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        let pass2_id = value["data"]["contribution_id"]
            .as_str()
            .unwrap()
            .to_string();

        let list = list_design_review_contributions_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            Query(ListDesignReviewContributionsQuery {
                round_id: Some("round-a".to_string()),
                phase: Some("pass_2".to_string()),
                role: Some("reviewer".to_string()),
                reviewed_version_id: None,
                read_set_contains: Some(pass1_id.clone()),
            }),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert_eq!(list.data.len(), 1);
        assert_eq!(list.data[0].contribution_id, pass2_id);

        let synthesis_response = create_design_review_synthesis_handler(
            State(state.clone()),
            Path((
                "demo".to_string(),
                artifact_id.clone(),
                workflow_run_id.clone(),
            )),
            mutation_headers("review-synthesis"),
            Json(DesignReviewSynthesisRequest {
                reviewed_version_id: None,
                read_set: serde_json::json!({ "contributions": [pass2_id] }),
                body_format: None,
                body: "# Synthesized design".to_string(),
                create_version: Some(true),
                version_label: Some("synthesis".to_string()),
                actor_display_name: Some("Codex Test".to_string()),
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(synthesis_response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        assert!(value["data"]["version"]["artifact_version_id"]
            .as_str()
            .is_some());
        assert_eq!(
            value["data"]["artifact"]["review_state"],
            "needs_user_decision"
        );
    }

    fn spec_fixture_manifest() -> Value {
        serde_json::json!({
            "spec_version": 1,
            "source_doc": "../gateway-features.md",
            "phases": [
                {
                    "id": "phase-1",
                    "name": "Phase 1",
                    "tasks": [
                        {
                            "id": "T001",
                            "team": "backend",
                            "title": "Create substrate contract",
                            "depends_on": [],
                            "labels": ["gateway-features", "phase-1"],
                            "touch_surface": ["docs/artifact-substrate-v1.md"],
                            "acceptance_criteria": ["contract is durable"],
                            "validation_plan": ["cargo test -p gateway"],
                            "spec_file": "backend/T001.md",
                            "status": "todo"
                        },
                        {
                            "id": "T002",
                            "team": "backend",
                            "title": "Implement task generation",
                            "depends_on": ["T001"],
                            "labels": ["gateway-features", "phase-1"],
                            "touch_surface": ["crates/gateway/src/routes.rs"],
                            "acceptance_criteria": ["tasks are generated idempotently"],
                            "validation_plan": ["cargo test -p gateway"],
                            "spec_file": "backend/T002.md",
                            "status": "todo"
                        }
                    ]
                }
            ]
        })
    }

    fn spec_fixture_files() -> std::collections::HashMap<String, String> {
        std::collections::HashMap::from([
            (
                "backend/T001.md".to_string(),
                "# T001\n\nFocused handoff for substrate.".to_string(),
            ),
            (
                "backend/T002.md".to_string(),
                "# T002\n\nFocused handoff for task generation.".to_string(),
            ),
        ])
    }

    async fn import_spec_fixture(state: &AppState, key: &str) -> (String, String) {
        let response = create_spec_handler(
            State(state.clone()),
            Path("demo".to_string()),
            mutation_headers(key),
            Json(SpecImportRequest {
                title: "Gateway Features".to_string(),
                labels: Some(vec!["gateway-features".to_string()]),
                body: Some("# Gateway Features Spec".to_string()),
                manifest: spec_fixture_manifest(),
                file_bodies: Some(spec_fixture_files()),
                source_doc: Some("../gateway-features.md".to_string()),
                source_artifact_id: None,
                source_artifact_version_id: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        (
            value["data"]["artifact"]["artifact_id"]
                .as_str()
                .unwrap()
                .to_string(),
            value["data"]["version"]["artifact_version_id"]
                .as_str()
                .unwrap()
                .to_string(),
        )
    }

    async fn accept_spec_fixture(state: &AppState, artifact_id: &str, version_id: &str) {
        let response = accept_spec_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.to_string())),
            mutation_headers("accept-spec-fixture"),
            Json(SpecAcceptRequest {
                version_id: version_id.to_string(),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(response).await;
        assert_eq!(status, StatusCode::OK, "{value}");
    }

    async fn generate_spec_tasks(
        state: &AppState,
        artifact_id: &str,
        key: &str,
        manifest_item_ids: Option<Vec<String>>,
    ) -> (StatusCode, Value) {
        let mut headers = mutation_headers(key);
        headers.extend(native_task_headers());
        let response = generate_spec_tasks_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.to_string())),
            headers,
            Json(GenerateSpecTasksRequest {
                confirmed: true,
                manifest_item_ids,
                reporter: Some("codex-test".to_string()),
                hostname: Some("test-host".to_string()),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        response_json(response).await
    }

    #[tokio::test]
    async fn spec_routes_import_manifest_round_trip_and_fetch_stable_item() {
        let state = test_state();
        let (artifact_id, version_id) = import_spec_fixture(&state, "spec-import-roundtrip").await;

        let manifest = get_spec_manifest_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            Query(SpecManifestQuery {
                version_id: Some(version_id.clone()),
            }),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert_eq!(manifest.0.data.artifact_version_id, version_id);
        assert_eq!(manifest.0.data.items.len(), 2);
        assert_eq!(manifest.0.data.items[0].manifest_item_id, "phase-1:T001");
        assert_eq!(
            manifest.0.data.items[0].spec_body.as_deref(),
            Some("# T001\n\nFocused handoff for substrate.")
        );
        assert_eq!(
            manifest.0.data.stability_policy["renamed"],
            "preserve manifest_item_id when the task id/code is unchanged"
        );

        let item = get_spec_manifest_item_handler(
            State(state),
            Path(("demo".to_string(), artifact_id, "phase-1:T002".to_string())),
            Query(SpecManifestQuery { version_id: None }),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert_eq!(item.0.data.task_code.as_deref(), Some("T002"));
        assert_eq!(
            item.0.data.acceptance_criteria,
            vec!["tasks are generated idempotently".to_string()]
        );
    }

    #[tokio::test]
    async fn spec_generate_tasks_uses_accepted_version_and_idempotent_rerun() {
        let state = test_state();
        let (artifact_id, version_id) = import_spec_fixture(&state, "spec-generate").await;
        accept_spec_fixture(&state, &artifact_id, &version_id).await;

        let (status, first) = generate_spec_tasks(&state, &artifact_id, "generate-run", None).await;
        assert_eq!(status, StatusCode::CREATED, "{first}");
        let first_tasks = first["data"]["generated_task_ids"].as_array().unwrap();
        let first_links = first["data"]["generated_link_ids"].as_array().unwrap();
        assert_eq!(first_tasks.len(), 2, "{first}");
        assert_eq!(first_links.len(), 2, "{first}");

        let (status, replay) =
            generate_spec_tasks(&state, &artifact_id, "generate-run", None).await;
        assert_eq!(status, StatusCode::OK, "{replay}");
        assert_eq!(replay["provenance"]["replay"], true);
        assert_eq!(
            replay["data"]["generated_task_ids"],
            first["data"]["generated_task_ids"]
        );
        assert_eq!(
            replay["data"]["generated_link_ids"],
            first["data"]["generated_link_ids"]
        );

        let task_id = first_tasks[0].as_str().unwrap();
        let detail = {
            let conn = state.db.lock().unwrap();
            db::get_task_detail(&conn, "demo", task_id)
                .unwrap()
                .unwrap()
        };
        let details = detail.task.details.unwrap();
        assert!(details.contains(&format!("source_spec_artifact_id: {artifact_id}")));
        assert!(details.contains(&format!("source_spec_version_id: {version_id}")));
        assert!(details.contains("manifest_item_id: phase-1:T001"));
        assert!(detail.comments.iter().any(|comment| {
            comment
                .content
                .contains("current task schema has no dedicated source fields")
        }));

        let links = {
            let conn = state.db.lock().unwrap();
            db::list_artifact_links(
                &conn,
                "demo",
                &db::ArtifactLinkFilters {
                    link_type: Some("task_generated_from_spec"),
                    source_kind: Some("artifact_version"),
                    source_id: Some(&version_id),
                    target_kind: Some("task"),
                    target_id: Some(task_id),
                },
            )
            .unwrap()
        };
        assert_eq!(links.len(), 1);
        assert_eq!(
            links[0].source_version_id.as_deref(),
            Some(version_id.as_str())
        );
        assert_eq!(
            links[0].source_child_address.as_deref(),
            Some("manifest.items[phase-1:T001]")
        );
        assert_eq!(links[0].target_kind, "task");
    }

    #[tokio::test]
    async fn spec_generate_tasks_recovers_existing_unlinked_task_after_partial_failure() {
        let state = test_state();
        let (artifact_id, version_id) = import_spec_fixture(&state, "spec-partial").await;
        accept_spec_fixture(&state, &artifact_id, &version_id).await;
        let orphan_task = {
            let conn = state.db.lock().unwrap();
            db::insert_task(
                &conn,
                "demo",
                "T001 Create substrate contract",
                Some("orphan from interrupted generation"),
                Some(&format!(
                    "Source:\nsource_spec_artifact_id: {artifact_id}\nsource_spec_version_id: {version_id}\nmanifest_item_id: phase-1:T001\n"
                )),
                &["generated-from-spec".to_string()],
                Some("test-host"),
                "codex-test",
            )
            .unwrap()
        };

        let (status, value) = generate_spec_tasks(
            &state,
            &artifact_id,
            "recover-partial",
            Some(vec!["phase-1:T001".to_string()]),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        assert_eq!(
            value["data"]["generated_task_ids"][0].as_str().unwrap(),
            orphan_task.id
        );
        let task_count: i64 = {
            let conn = state.db.lock().unwrap();
            conn.query_row(
                "SELECT COUNT(*) FROM tasks WHERE project_ident = 'demo'",
                [],
                |row| row.get(0),
            )
            .unwrap()
        };
        assert_eq!(
            task_count, 1,
            "rerun should link existing task, not duplicate it"
        );
    }

    #[tokio::test]
    async fn spec_link_existing_task_creates_back_link() {
        let state = test_state();
        let (artifact_id, version_id) = import_spec_fixture(&state, "spec-link-existing").await;
        accept_spec_fixture(&state, &artifact_id, &version_id).await;
        let task = {
            let conn = state.db.lock().unwrap();
            db::insert_task(
                &conn,
                "demo",
                "Existing implementation task",
                None,
                Some("pre-existing handoff"),
                &[],
                None,
                "codex-test",
            )
            .unwrap()
        };
        let response = link_existing_spec_task_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("link-existing-task"),
            Json(LinkSpecTaskRequest {
                version_id: None,
                manifest_item_id: "phase-1:T002".to_string(),
                task_id: task.id.clone(),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        assert_eq!(value["data"]["target_id"].as_str().unwrap(), task.id);
        assert_eq!(
            value["data"]["source_child_address"].as_str().unwrap(),
            "manifest.items[phase-1:T002]"
        );
    }

    #[tokio::test]
    async fn artifact_routes_cover_happy_path_and_idempotent_retry() {
        let state = test_state();
        let artifact_id = create_test_artifact(&state, "artifact-create-1").await;
        let run_id = start_test_run(&state, &artifact_id, "version-run-1").await;

        let (status, first_version) = create_test_version(
            &state,
            &artifact_id,
            "version-1",
            Some(&run_id),
            None,
            "hello",
        )
        .await;
        assert_eq!(status, StatusCode::CREATED, "{first_version}");
        let version_id = first_version["data"]["artifact_version_id"]
            .as_str()
            .unwrap()
            .to_string();

        let (status, replayed_version) = create_test_version(
            &state,
            &artifact_id,
            "version-1",
            Some(&run_id),
            None,
            "hello",
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{replayed_version}");
        assert_eq!(replayed_version["provenance"]["replay"], true);
        assert_eq!(
            replayed_version["data"]["artifact_version_id"],
            first_version["data"]["artifact_version_id"]
        );

        let (status, second_version) = create_test_version(
            &state,
            &artifact_id,
            "version-2",
            Some(&run_id),
            Some(&version_id),
            "hello\nnew",
        )
        .await;
        assert_eq!(status, StatusCode::CREATED, "{second_version}");
        let second_version_id = second_version["data"]["artifact_version_id"]
            .as_str()
            .unwrap()
            .to_string();

        let diff = diff_artifact_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone(), second_version_id)),
            Query(DiffQuery {
                base_version_id: Some(version_id.clone()),
            }),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert!(diff.0.data.diff.contains("+new"));
        assert_eq!(diff.0.chunking_status.status, "none");

        let comment_response = create_artifact_comment_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("comment-1"),
            Json(CreateCommentRequest {
                target_kind: "artifact".to_string(),
                target_id: artifact_id.clone(),
                child_address: None,
                parent_comment_id: None,
                body: "Looks good".to_string(),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(response_json(comment_response).await.0, StatusCode::CREATED);

        let contribution_response = create_artifact_contribution_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("contribution-1"),
            Json(CreateContributionRequest {
                target_kind: "artifact".to_string(),
                target_id: artifact_id.clone(),
                contribution_kind: "note".to_string(),
                phase: None,
                role: "author".to_string(),
                read_set: None,
                body_format: Some("markdown".to_string()),
                body: "Implementation note".to_string(),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(
            response_json(contribution_response).await.0,
            StatusCode::CREATED
        );

        let link_response = create_artifact_link_handler(
            State(state.clone()),
            Path("demo".to_string()),
            mutation_headers("link-1"),
            Json(CreateLinkRequest {
                link_type: "doc_referenced_by_spec".to_string(),
                source_kind: "artifact".to_string(),
                source_id: artifact_id.clone(),
                source_version_id: None,
                source_child_address: None,
                target_kind: "artifact_version".to_string(),
                target_id: version_id.clone(),
                target_version_id: Some(version_id),
                target_child_address: None,
                supersedes_link_id: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(response_json(link_response).await.0, StatusCode::CREATED);

        let fetched = get_artifact_handler(
            State(state),
            Path(("demo".to_string(), artifact_id)),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert!(fetched.0.data.current_version.is_some());
        assert_eq!(fetched.0.chunking_status.status, "none");
    }

    #[tokio::test]
    async fn artifact_authorization_enforces_project_and_scopes_when_hardened() {
        let state = hardened_test_state();

        let missing_project = create_artifact_handler(
            State(state.clone()),
            Path("demo".to_string()),
            mutation_headers("auth-missing-project"),
            Json(CreateArtifactRequest {
                kind: "spec".to_string(),
                subkind: None,
                title: "Unauthorized".to_string(),
                labels: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(missing_project.0, StatusCode::FORBIDDEN);
        assert!(missing_project.1.contains("x-agent-project"));

        let missing_scope = create_artifact_handler(
            State(state.clone()),
            Path("demo".to_string()),
            authorized_headers("auth-missing-scope", "artifact.read"),
            Json(CreateArtifactRequest {
                kind: "spec".to_string(),
                subkind: None,
                title: "Unauthorized".to_string(),
                labels: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(missing_scope.0, StatusCode::FORBIDDEN);
        assert!(missing_scope.1.contains("artifact.write"));

        let response = create_artifact_handler(
            State(state.clone()),
            Path("Demo".to_string()),
            authorized_headers("auth-create", "artifact.write"),
            Json(CreateArtifactRequest {
                kind: "spec".to_string(),
                subkind: None,
                title: "Authorized".to_string(),
                labels: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        let (status, value) = response_json(response).await;
        assert_eq!(status, StatusCode::CREATED, "{value}");
        assert_eq!(
            value["provenance"]["authorization"]["boundary"],
            "project-scoped"
        );
        assert_eq!(
            value["provenance"]["authorization"]["required_scopes"][0],
            "artifact.write"
        );
        let artifact_id = value["data"]["artifact_id"].as_str().unwrap().to_string();

        let read_reject = get_artifact_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            HeaderMap::new(),
        )
        .await
        .unwrap_err();
        assert_eq!(read_reject.0, StatusCode::FORBIDDEN);

        let fetched = get_artifact_handler(
            State(state),
            Path(("DEMO".to_string(), artifact_id)),
            read_headers("artifact.read"),
        )
        .await
        .unwrap();
        assert_eq!(fetched.0.data.artifact.title, "Authorized");
        assert_eq!(fetched.0.data.artifact.project_ident, "demo");
    }

    #[tokio::test]
    async fn artifact_quota_override_requires_project_administer_when_hardened() {
        let state = hardened_test_state();

        let mut non_admin = authorized_headers("quota-override-forbidden", "artifact.write");
        non_admin.insert(
            "x-artifact-quota-override",
            HeaderValue::from_static("true"),
        );
        let err = create_artifact_handler(
            State(state.clone()),
            Path("demo".to_string()),
            non_admin,
            Json(CreateArtifactRequest {
                kind: "spec".to_string(),
                subkind: None,
                title: "Override denied".to_string(),
                labels: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(err.0, StatusCode::FORBIDDEN);
        assert!(err.1.contains("project.administer"));

        let mut admin =
            authorized_headers("quota-override-admin", "artifact.write project.administer");
        admin.insert(
            "x-artifact-quota-override",
            HeaderValue::from_static("true"),
        );
        let response = create_artifact_handler(
            State(state),
            Path("demo".to_string()),
            admin,
            Json(CreateArtifactRequest {
                kind: "spec".to_string(),
                subkind: None,
                title: "Override allowed".to_string(),
                labels: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(response_json(response).await.0, StatusCode::CREATED);
    }

    #[tokio::test]
    async fn artifact_routes_validate_actor_idempotency_and_immutable_refs() {
        let state = test_state();
        let artifact_id = create_test_artifact(&state, "artifact-create-validation").await;

        let mut missing_agent = mutation_headers("missing-agent");
        missing_agent.remove("x-agent-id");
        let missing_agent_error = create_artifact_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            missing_agent,
            Json(CreateArtifactVersionRequest {
                version_label: None,
                parent_version_id: None,
                body_format: Some("markdown".to_string()),
                body: Some("body".to_string()),
                structured_payload: None,
                source_format: None,
                version_state: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(missing_agent_error.0, StatusCode::BAD_REQUEST);
        assert_eq!(missing_agent_error.1, "x_agent_id_required");

        let missing_parent = create_artifact_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("missing-parent"),
            Json(CreateArtifactVersionRequest {
                version_label: None,
                parent_version_id: Some("missing-version".to_string()),
                body_format: Some("markdown".to_string()),
                body: Some("body".to_string()),
                structured_payload: None,
                source_format: None,
                version_state: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(missing_parent.0, StatusCode::BAD_REQUEST);

        let read_set_error = create_artifact_contribution_handler(
            State(state),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("read-set-required"),
            Json(CreateContributionRequest {
                target_kind: "artifact".to_string(),
                target_id: artifact_id,
                contribution_kind: "synthesis".to_string(),
                phase: Some("synthesis".to_string()),
                role: "analyst".to_string(),
                read_set: None,
                body_format: Some("markdown".to_string()),
                body: "synthesis".to_string(),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(read_set_error.0, StatusCode::BAD_REQUEST);
        assert_eq!(read_set_error.1, "read_set_required");
    }

    #[tokio::test]
    async fn artifact_routes_return_stable_t004_size_limit_errors() {
        let state = test_state_with_operations(t008_operations_fixture());
        let artifact_id = create_test_artifact(&state, "t008-size-artifact").await;

        let oversized_version = create_artifact_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("t008-oversized-version"),
            Json(CreateArtifactVersionRequest {
                version_label: None,
                parent_version_id: None,
                body_format: Some("markdown".to_string()),
                body: Some("x".repeat(4097)),
                structured_payload: None,
                source_format: None,
                version_state: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(oversized_version.0, StatusCode::PAYLOAD_TOO_LARGE);
        assert_eq!(oversized_version.1, "artifact_version_body_too_large");

        let oversized_contribution = create_artifact_contribution_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("t008-oversized-contribution"),
            Json(CreateContributionRequest {
                target_kind: "artifact".to_string(),
                target_id: artifact_id.clone(),
                contribution_kind: "note".to_string(),
                phase: None,
                role: "author".to_string(),
                read_set: None,
                body_format: Some("markdown".to_string()),
                body: "x".repeat(2049),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(oversized_contribution.0, StatusCode::PAYLOAD_TOO_LARGE);
        assert_eq!(oversized_contribution.1, "contribution_body_too_large");

        let oversized_comment = create_artifact_comment_handler(
            State(state),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("t008-oversized-comment"),
            Json(CreateCommentRequest {
                target_kind: "artifact".to_string(),
                target_id: artifact_id,
                child_address: None,
                parent_comment_id: None,
                body: "x".repeat(513),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(oversized_comment.0, StatusCode::PAYLOAD_TOO_LARGE);
        assert_eq!(oversized_comment.1, "comment_body_too_large");
    }

    #[tokio::test]
    async fn artifact_routes_surface_soft_quota_warning_and_hard_reject() {
        let state = test_state_with_operations(t008_operations_fixture());

        async fn post_artifact(
            state: &AppState,
            key: &str,
            title: &str,
        ) -> std::result::Result<Response, AppError> {
            create_artifact_handler(
                State(state.clone()),
                Path("demo".to_string()),
                mutation_headers(key),
                Json(CreateArtifactRequest {
                    kind: "spec".to_string(),
                    subkind: None,
                    title: title.to_string(),
                    labels: Some(vec!["qa".to_string()]),
                    actor_display_name: None,
                }),
            )
            .await
        }

        for idx in 1..=2 {
            let response = post_artifact(
                &state,
                &format!("t008-quota-{idx}"),
                &format!("quota fixture {idx}"),
            )
            .await
            .unwrap();
            let (status, value) = response_json(response).await;
            assert_eq!(status, StatusCode::CREATED, "{value}");
            assert_eq!(value["provenance"]["warnings"].as_array().unwrap().len(), 0);
        }

        let soft_response = post_artifact(&state, "t008-quota-3", "quota fixture 3")
            .await
            .unwrap();
        let (status, soft_value) = response_json(soft_response).await;
        assert_eq!(status, StatusCode::CREATED, "{soft_value}");
        assert_eq!(
            soft_value["provenance"]["warnings"][0],
            "quota_artifact_soft"
        );

        let hard_reject = post_artifact(&state, "t008-quota-4", "quota fixture 4")
            .await
            .unwrap_err();
        assert_eq!(hard_reject.0, StatusCode::TOO_MANY_REQUESTS);
        assert_eq!(hard_reject.1, "quota_artifact_exceeded");

        let count: i64 = {
            let conn = state.db.lock().unwrap();
            conn.query_row(
                "SELECT COUNT(*) FROM artifacts WHERE project_ident = 'demo'",
                [],
                |row| row.get(0),
            )
            .unwrap()
        };
        assert_eq!(count, 3, "hard quota reject must not insert a row");
    }

    #[tokio::test]
    async fn artifact_routes_expose_stale_and_partial_chunking_status() {
        let state = test_state();
        let artifact_id = create_test_artifact(&state, "t008-chunks-artifact").await;
        let (status, v1) =
            create_test_version(&state, &artifact_id, "t008-chunks-v1", None, None, "v1").await;
        assert_eq!(status, StatusCode::CREATED, "{v1}");
        let v1_id = v1["data"]["artifact_version_id"]
            .as_str()
            .unwrap()
            .to_string();
        let (status, v2) = create_test_version(
            &state,
            &artifact_id,
            "t008-chunks-v2",
            None,
            Some(&v1_id),
            "v2",
        )
        .await;
        assert_eq!(status, StatusCode::CREATED, "{v2}");
        let v2_id = v2["data"]["artifact_version_id"]
            .as_str()
            .unwrap()
            .to_string();

        {
            let conn = state.db.lock().unwrap();
            db::create_artifact_chunk(
                &conn,
                &state.artifact_operations,
                &db::ArtifactChunkInsert {
                    artifact_id: &artifact_id,
                    artifact_version_id: &v1_id,
                    child_address: "manifest.items[T008-stale]",
                    text: "stale chunk",
                    embedding_model: None,
                    embedding_vector: None,
                    app: Some("spec"),
                    label: Some("qa"),
                    kind: Some("manifest_item"),
                    metadata: Some(&serde_json::json!({"status": "ok"})),
                },
            )
            .unwrap();
        }

        let stale = get_artifact_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert_eq!(stale.0.chunking_status.status, "stale");
        assert_eq!(stale.0.chunking_status.current_chunk_count, 0);
        assert_eq!(stale.0.chunking_status.stale_chunk_count, 1);
        assert_eq!(stale.0.chunking_status.superseded_chunk_count, 0);
        assert!(stale.0.chunking_status.failed_addresses.is_empty());

        {
            let conn = state.db.lock().unwrap();
            db::create_artifact_chunk(
                &conn,
                &state.artifact_operations,
                &db::ArtifactChunkInsert {
                    artifact_id: &artifact_id,
                    artifact_version_id: &v2_id,
                    child_address: "manifest.items[T008-partial]",
                    text: "partial chunk",
                    embedding_model: None,
                    embedding_vector: None,
                    app: Some("spec"),
                    label: Some("qa"),
                    kind: Some("manifest_item"),
                    metadata: Some(&serde_json::json!({"status": "failed"})),
                },
            )
            .unwrap();
        }

        let partial_version = get_artifact_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone(), v2_id.clone())),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert_eq!(partial_version.0.chunking_status.status, "partial");
        assert_eq!(partial_version.0.data.chunking_status.status, "partial");
        assert_eq!(
            partial_version.0.chunking_status.failed_addresses,
            vec!["manifest.items[T008-partial]".to_string()]
        );

        let partial_diff = diff_artifact_version_handler(
            State(state),
            Path(("demo".to_string(), artifact_id, v2_id)),
            Query(DiffQuery {
                base_version_id: Some(v1_id),
            }),
            HeaderMap::new(),
        )
        .await
        .unwrap();
        assert_eq!(partial_diff.0.chunking_status.status, "partial");
        assert_eq!(partial_diff.0.data.chunking_status.status, "partial");
        assert_eq!(
            partial_diff.0.data.chunking_status.failed_addresses,
            vec!["manifest.items[T008-partial]".to_string()]
        );
    }

    #[tokio::test]
    async fn artifact_mutation_helpers_preserve_replay_envelope_after_consolidation() {
        let state = test_state();
        let artifact_id = create_test_artifact(&state, "t030-artifact").await;
        let run_id = start_test_run(&state, &artifact_id, "t030-run").await;

        // 1. version: idempotent retry must return OK + replay=true + same id.
        //    Versions' idempotency index is keyed on
        //    (artifact_id, created_via_workflow_run_id, idempotency_key) so we
        //    must hold the workflow_run_id constant across both calls.
        let (first_status, first_version) = create_test_version(
            &state,
            &artifact_id,
            "t030-version",
            Some(&run_id),
            None,
            "body-a",
        )
        .await;
        assert_eq!(first_status, StatusCode::CREATED, "{first_version}");
        assert_eq!(first_version["provenance"]["replay"], false);
        let version_id = first_version["data"]["artifact_version_id"]
            .as_str()
            .unwrap()
            .to_string();

        let (replay_status, replay_version) = create_test_version(
            &state,
            &artifact_id,
            "t030-version",
            Some(&run_id),
            None,
            "body-a",
        )
        .await;
        assert_eq!(replay_status, StatusCode::OK, "{replay_version}");
        assert_eq!(replay_version["provenance"]["replay"], true);
        assert_eq!(
            replay_version["data"]["artifact_version_id"]
                .as_str()
                .unwrap(),
            version_id
        );
        assert_eq!(
            replay_version["provenance"]["generated_resources"]["artifact_version_id"]
                .as_str()
                .unwrap(),
            version_id
        );
        // Authorization scopes must be preserved by `finalize_mutation`.
        assert_eq!(
            replay_version["provenance"]["authorization"]["required_scopes"][0],
            "artifact_version.create"
        );

        // 2. contribution: same replay invariants through the consolidated helper.
        async fn post_contribution(
            state: &AppState,
            artifact_id: &str,
            key: &str,
        ) -> (StatusCode, Value) {
            let response = create_artifact_contribution_handler(
                State(state.clone()),
                Path(("demo".to_string(), artifact_id.to_string())),
                mutation_headers(key),
                Json(CreateContributionRequest {
                    target_kind: "artifact".to_string(),
                    target_id: artifact_id.to_string(),
                    contribution_kind: "note".to_string(),
                    phase: None,
                    role: "author".to_string(),
                    read_set: None,
                    body_format: Some("markdown".to_string()),
                    body: "consolidated".to_string(),
                    actor_display_name: None,
                }),
            )
            .await
            .unwrap();
            response_json(response).await
        }
        let (status, first_contrib) = post_contribution(&state, &artifact_id, "t030-contrib").await;
        assert_eq!(status, StatusCode::CREATED);
        let contrib_id = first_contrib["data"]["contribution_id"]
            .as_str()
            .unwrap()
            .to_string();
        let (status, replay_contrib) =
            post_contribution(&state, &artifact_id, "t030-contrib").await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(replay_contrib["provenance"]["replay"], true);
        assert_eq!(
            replay_contrib["data"]["contribution_id"].as_str().unwrap(),
            contrib_id
        );

        // 3. link: project-scoped helper must apply the same idempotency
        //    semantics. Links only carry idempotency uniqueness when a
        //    workflow_run_id is supplied (per the DB unique-index condition),
        //    so set `x-workflow-run-id` to exercise the replay path through
        //    `load_workflow_run_for_link`.
        async fn post_link(
            state: &AppState,
            artifact_id: &str,
            version_id: &str,
            run_id: &str,
            key: &str,
        ) -> (StatusCode, Value) {
            let mut headers = mutation_headers(key);
            headers.insert("x-workflow-run-id", HeaderValue::from_str(run_id).unwrap());
            let response = create_artifact_link_handler(
                State(state.clone()),
                Path("demo".to_string()),
                headers,
                Json(CreateLinkRequest {
                    link_type: "doc_referenced_by_spec".to_string(),
                    source_kind: "artifact".to_string(),
                    source_id: artifact_id.to_string(),
                    source_version_id: None,
                    source_child_address: None,
                    target_kind: "artifact_version".to_string(),
                    target_id: version_id.to_string(),
                    target_version_id: Some(version_id.to_string()),
                    target_child_address: None,
                    supersedes_link_id: None,
                    actor_display_name: None,
                }),
            )
            .await
            .unwrap();
            response_json(response).await
        }
        let (status, first_link) =
            post_link(&state, &artifact_id, &version_id, &run_id, "t030-link").await;
        assert_eq!(status, StatusCode::CREATED);
        let link_id = first_link["data"]["link_id"].as_str().unwrap().to_string();
        let (status, replay_link) =
            post_link(&state, &artifact_id, &version_id, &run_id, "t030-link").await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(replay_link["provenance"]["replay"], true);
        assert_eq!(replay_link["data"]["link_id"].as_str().unwrap(), link_id);
        assert_eq!(
            replay_link["provenance"]["authorization"]["required_scopes"][0],
            "link.write"
        );
    }

    #[tokio::test]
    async fn artifact_routes_allow_resumable_failed_run_retry_and_reject_cancelled() {
        let state = test_state();
        let artifact_id = create_test_artifact(&state, "artifact-create-workflow").await;
        let (_, version) = create_test_version(
            &state,
            &artifact_id,
            "workflow-source-version",
            None,
            None,
            "source",
        )
        .await;
        let source_version_id = version["data"]["artifact_version_id"]
            .as_str()
            .unwrap()
            .to_string();

        let started = start_artifact_workflow_run_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("run-resumable"),
            Json(StartWorkflowRunRequest {
                workflow_kind: "spec_task_generation".to_string(),
                phase: Some("generation".to_string()),
                round_id: None,
                participant_actor_ids: None,
                source_artifact_version_id: Some(source_version_id.clone()),
                read_set: None,
                is_resumable: Some(true),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        let (_, started_json) = response_json(started).await;
        let run_id = started_json["data"]["workflow_run_id"]
            .as_str()
            .unwrap()
            .to_string();

        let failed = complete_artifact_workflow_run_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone(), run_id.clone())),
            mutation_headers("run-resumable-fail"),
            Json(CompleteWorkflowRunRequest {
                state: "failed".to_string(),
                failure_reason: Some("transient".to_string()),
                generated_contribution_ids: None,
                generated_version_ids: None,
                generated_task_ids: None,
                generated_link_ids: None,
                generated_chunk_ids: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(response_json(failed).await.0, StatusCode::OK);

        let mut retry_headers = mutation_headers("version-after-failed-run");
        retry_headers.insert("x-workflow-run-id", HeaderValue::from_str(&run_id).unwrap());
        let recovered = create_artifact_version_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            retry_headers,
            Json(CreateArtifactVersionRequest {
                version_label: Some("resumed".to_string()),
                parent_version_id: Some(source_version_id.clone()),
                body_format: Some("markdown".to_string()),
                body: Some("recovered".to_string()),
                structured_payload: None,
                source_format: None,
                version_state: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(response_json(recovered).await.0, StatusCode::CREATED);

        let cancelled = start_artifact_workflow_run_handler(
            State(state.clone()),
            Path(("demo".to_string(), artifact_id.clone())),
            mutation_headers("run-cancelled"),
            Json(StartWorkflowRunRequest {
                workflow_kind: "spec_task_generation".to_string(),
                phase: None,
                round_id: None,
                participant_actor_ids: None,
                source_artifact_version_id: Some(source_version_id.clone()),
                read_set: None,
                is_resumable: Some(true),
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        let (_, cancelled_json) = response_json(cancelled).await;
        let cancelled_run_id = cancelled_json["data"]["workflow_run_id"]
            .as_str()
            .unwrap()
            .to_string();
        let cancelled_done = complete_artifact_workflow_run_handler(
            State(state.clone()),
            Path((
                "demo".to_string(),
                artifact_id.clone(),
                cancelled_run_id.clone(),
            )),
            mutation_headers("run-cancelled-complete"),
            Json(CompleteWorkflowRunRequest {
                state: "cancelled".to_string(),
                failure_reason: None,
                generated_contribution_ids: None,
                generated_version_ids: None,
                generated_task_ids: None,
                generated_link_ids: None,
                generated_chunk_ids: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap();
        assert_eq!(response_json(cancelled_done).await.0, StatusCode::OK);

        let mut cancelled_retry_headers = mutation_headers("version-after-cancelled-run");
        cancelled_retry_headers.insert(
            "x-workflow-run-id",
            HeaderValue::from_str(&cancelled_run_id).unwrap(),
        );
        let rejected = create_artifact_version_handler(
            State(state),
            Path(("demo".to_string(), artifact_id)),
            cancelled_retry_headers,
            Json(CreateArtifactVersionRequest {
                version_label: Some("cancelled".to_string()),
                parent_version_id: Some(source_version_id),
                body_format: Some("markdown".to_string()),
                body: Some("must reject".to_string()),
                structured_payload: None,
                source_format: None,
                version_state: None,
                actor_display_name: None,
            }),
        )
        .await
        .unwrap_err();
        assert_eq!(rejected.0, StatusCode::BAD_REQUEST);
    }
}
