//! Completion reviews are ordinary queued tasks, created atomically once.
use crate::{db, routes::AppError, task_dispatch::Conflict, task_events, AppState};
use anyhow::Result;
use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::{IntoResponse, Response},
    Json,
};
use rusqlite::{params, Connection, OptionalExtension};

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS task_reviews (
        source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        source_done_at INTEGER NOT NULL,
        review_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        PRIMARY KEY(source_task_id,source_done_at));",
    )?;
    Ok(())
}

pub fn ensure_reviewer(
    labels: &[String],
    origin: Option<&task_events::AgentOrigin>,
    actor_agent_id: Option<&str>,
) -> Result<()> {
    if labels
        .iter()
        .filter_map(|label| label.strip_prefix("review-exclude:"))
        .any(|excluded| {
            origin.is_some_and(|origin| origin.session_id == excluded)
                || actor_agent_id == Some(excluded)
        })
    {
        return Err(Conflict("completion reviews require a different agent").into());
    }
    Ok(())
}

fn opposite(provider: &str) -> Option<&'static str> {
    match provider {
        "claude" => Some("codex"),
        "codex" => Some("claude"),
        _ => None,
    }
}

fn preferred_for_source(conn: &Connection, source: &str) -> Result<Option<String>> {
    let provider: Option<String> = conn
        .query_row(
            "SELECT COALESCE(json_extract(owner_origin_json,'$.provider'),
                (SELECT json_extract(origin_json,'$.provider') FROM task_comments
                 WHERE task_id=tasks.id AND author_type='agent'
                 AND json_extract(origin_json,'$.provider') IS NOT NULL
                 ORDER BY created_at DESC,id DESC LIMIT 1)) FROM tasks WHERE id=?1",
            params![source],
            |row| row.get(0),
        )
        .optional()?
        .flatten();
    Ok(provider.as_deref().and_then(opposite).map(str::to_owned))
}

/// New reviews freeze the desired provider; older queued reviews resolve it
/// from their source task without requiring a data migration.
fn preferred_provider(conn: &Connection, labels: &[String]) -> Result<Option<String>> {
    if !labels.iter().any(|label| label == "task-review") {
        return Ok(None);
    }
    if let Some(provider) = labels
        .iter()
        .find_map(|label| label.strip_prefix("review-provider:"))
    {
        return Ok(Some(provider.to_owned()));
    }
    let Some(source) = labels
        .iter()
        .find_map(|label| label.strip_prefix("review-of:"))
    else {
        return Ok(None);
    };
    preferred_for_source(conn, source)
}

/// Used by the controller and every claim/reassignment path. A busy or
/// incompatible opposite-provider registration still requires waiting for it.
pub fn ensure_provider(
    conn: &Connection,
    labels: &[String],
    origin: Option<&task_events::AgentOrigin>,
    actor_agent_id: Option<&str>,
) -> Result<()> {
    // A human board status change nominates no reviewer. Explicit owner
    // assignments still supply an actor ID or target origin and are checked.
    if origin.is_none() && actor_agent_id.is_none() {
        return Ok(());
    }
    let Some(preferred) = preferred_provider(conn, labels)? else {
        return Ok(());
    };
    if !crate::plugins::presence::provider_registered(conn, &preferred, db::now_ms())? {
        return Ok(());
    }
    let provider = if let Some(origin) = origin {
        Some(origin.provider.clone())
    } else if let Some(actor) = actor_agent_id {
        conn.query_row(
            "SELECT json_extract(origin_json,'$.provider') FROM task_target_presence
             WHERE session_id=?1 OR json_extract(agent_json,'$.agent_id')=?1
             ORDER BY COALESCE(report_seen_at,seen_at) DESC LIMIT 1",
            params![actor],
            |row| row.get::<_, String>(0),
        )
        .optional()?
    } else {
        None
    };
    if provider.as_deref() != Some(preferred.as_str()) {
        return Err(Conflict("completion reviews require the opposite provider while it is registered; use an eligible agent identity").into());
    }
    Ok(())
}

pub fn preserve_metadata(current: &[String], next: &[String]) -> Result<()> {
    if current.iter().any(|label| label == "task-review") {
        let markers = |labels: &[String]| {
            labels
                .iter()
                .filter(|label| {
                    label.as_str() == "task-review"
                        || label.starts_with("review-of:")
                        || label.starts_with("review-exclude:")
                        || label.starts_with("review-provider:")
                })
                .cloned()
                .collect::<std::collections::BTreeSet<_>>()
        };
        if markers(current) != markers(next) {
            return Err(Conflict(
                "completion review source, exclusions and provider preference are immutable",
            )
            .into());
        }
    }
    Ok(())
}

/// Caller supplies the existing task-mutation transaction and DB mutex.
pub fn enqueue(conn: &Connection, project: &str, source_id: &str) -> Result<Option<db::Task>> {
    let source = db::get_task_detail(conn, project, source_id)?
        .ok_or(Conflict("source task not found"))?
        .task;
    if source.labels.iter().any(|label| label == "task-review")
        || (source.kind == "delegated" && source.delegated_to_task_id.is_some())
    {
        return Ok(None);
    }
    if source.status != "done" {
        return Err(Conflict("source task is not completed").into());
    }
    let completed = source
        .done_at
        .ok_or(Conflict("source completion timestamp is missing"))?;
    let existing: Option<String> = conn
        .query_row(
            "SELECT review_task_id FROM task_reviews WHERE source_task_id=?1 AND source_done_at=?2",
            params![source.id, completed],
            |row| row.get(0),
        )
        .optional()?;
    if let Some(id) = existing {
        return Ok(db::get_task_detail(conn, project, &id)?.map(|detail| detail.task));
    }
    let mut labels = vec!["task-review".to_owned(), format!("review-of:{}", source.id)];
    if let Some(provider) = preferred_for_source(conn, &source.id)? {
        labels.push(format!("review-provider:{provider}"));
    }
    labels.extend(
        source
            .labels
            .iter()
            .filter(|label| label.starts_with("os:"))
            .cloned(),
    );
    if let Some(owner) = source
        .owner_origin
        .as_ref()
        .map(|owner| owner.session_id.as_str())
        .or(source.owner_agent_id.as_deref())
    {
        labels.push(format!("review-exclude:{owner}"));
    }
    // Agents that commented may have done the work before ownership changed.
    let contributors = conn
        .prepare(
            "SELECT json_extract(origin_json,'$.session_id') FROM task_comments
             WHERE task_id=?1 AND author_type='agent'
               AND json_extract(origin_json,'$.session_id') IS NOT NULL
             ORDER BY created_at",
        )?
        .query_map(params![source.id], |row| row.get::<_, String>(0))?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    for session in contributors {
        let label = format!("review-exclude:{session}");
        if !labels.contains(&label) {
            labels.push(label);
        }
    }
    let specification = crate::templates::render(
        crate::templates::REVIEW_SPECIFICATION,
        &[
            ("source_task_id", &source.id),
            ("source_title", &source.title),
        ],
    );
    // This helper only inserts an unowned ordinary task; no dispatch protocol
    // record or submitting-agent claim is created.
    let task = db::insert_unassigned_dispatch_task(
        conn,
        project,
        &format!("Review: {}", source.title),
        None,
        Some(&specification),
        &labels,
        sysinfo::System::host_name().as_deref(),
        "amux-autoreview",
    )?;
    conn.execute(
        "INSERT INTO task_reviews VALUES(?1,?2,?3)",
        params![source.id, completed, task.id],
    )?;
    Ok(Some(task))
}

pub async fn create(
    State(state): State<AppState>,
    Path((project, id)): Path<(String, String)>,
) -> Result<Response, AppError> {
    let task = tokio::task::spawn_blocking(move || -> Result<_> {
        let conn = state.db.lock().unwrap();
        task_events::mutation(&conn, None, |conn| enqueue(conn, &project, &id))
    })
    .await??;
    Ok(match task {
        Some(task) => Json(task).into_response(),
        None => StatusCode::NO_CONTENT.into_response(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    fn actor() -> task_events::AgentOrigin {
        task_events::AgentOrigin {
            session_id: uuid::Uuid::now_v7().to_string(),
            instance_id: uuid::Uuid::now_v7().to_string(),
            provider: "codex".into(),
            os: "linux".into(),
        }
    }
    fn update(
        conn: &Connection,
        id: &str,
        status: &str,
        actor: &task_events::AgentOrigin,
    ) -> Result<db::Task> {
        task_events::mutation(conn, Some(actor), |conn| {
            db::update_task(
                conn,
                "example",
                id,
                &db::TaskUpdate {
                    status: Some(status),
                    goal: None,
                    owner_agent_id: None,
                    rank: None,
                    title: None,
                    description: None,
                    details: None,
                    labels: None,
                    hostname: None,
                },
                Some(&actor.session_id),
            )?
            .ok_or_else(|| anyhow::anyhow!("missing fixture task"))
        })
    }
    fn fixture() -> (db::Db, db::Task, task_events::AgentOrigin) {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES ('example','none','',0)", []).unwrap();
        let author = actor();
        let source = task_events::mutation(&conn, Some(&author), |conn| {
            db::insert_task(
                conn,
                "example",
                "Fix fixture",
                None,
                None,
                &["os:linux".into()],
                None,
                "author",
            )
        })
        .unwrap();
        update(&conn, &source.id, "in_progress", &author).unwrap();
        let source = update(&conn, &source.id, "done", &author).unwrap();
        drop(conn);
        (database, source, author)
    }
    #[test]
    fn review_is_unowned_deduplicated_independent_and_non_recursive() {
        let (database, source, author) = fixture();
        let conn = database.lock().unwrap();
        let review =
            task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &source.id))
                .unwrap()
                .unwrap();
        assert_eq!(review.status, "todo");
        assert_eq!(review.hostname, sysinfo::System::host_name());
        assert!(review.owner_origin.is_none() && review.owner_agent_id.is_none());
        assert!(review.labels.contains(&"os:linux".into()));
        assert!(review.labels.contains(&"review-provider:claude".into()));
        assert!(update(&conn, &review.id, "in_progress", &author).is_err());
        assert_eq!(
            task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &source.id))
                .unwrap()
                .unwrap()
                .id,
            review.id
        );
        let peer = actor();
        update(&conn, &review.id, "in_progress", &peer).unwrap();
        update(&conn, &review.id, "done", &peer).unwrap();
        assert!(
            task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &review.id))
                .unwrap()
                .is_none()
        );
        assert_eq!(
            task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &source.id))
                .unwrap()
                .unwrap()
                .id,
            review.id
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM task_reviews", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            1
        );
    }
    #[test]
    fn registered_opposite_provider_blocks_manual_claims_even_when_busy_elsewhere() {
        let (database, source, _) = fixture();
        let conn = database.lock().unwrap();
        let review =
            task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &source.id))
                .unwrap()
                .unwrap();
        let peer = actor();
        let mut reviewer = actor();
        reviewer.provider = "claude".into();
        // Register on another project/OS, busy with no idle evidence. This
        // still prevents same-provider fallback, including direct REST claims.
        let now = db::now_ms();
        crate::plugins::presence::seed_report(
            &conn,
            "fixture",
            serde_json::json!({
                "session_id":reviewer.session_id,"instance_id":reviewer.instance_id,
                "provider":"claude","os":"windows","project":"another-project"
            }),
            now,
            Some(now),
            true,
            "busy",
        );
        assert!(update(&conn, &review.id, "in_progress", &peer).is_err());
        assert!(
            ensure_provider(&conn, &review.labels, None, Some("unknown-legacy-agent")).is_err()
        );
        assert!(ensure_provider(&conn, &review.labels, None, Some(&reviewer.session_id)).is_ok());
        // Existing reviews without the new marker follow the same policy.
        let legacy: Vec<_> = review
            .labels
            .iter()
            .filter(|label| !label.starts_with("review-provider:"))
            .cloned()
            .collect();
        assert!(ensure_provider(&conn, &legacy, Some(&peer), None).is_err());
        let stripped: Vec<_> = review
            .labels
            .iter()
            .filter(|label| !label.starts_with("review-provider:"))
            .cloned()
            .collect();
        assert!(preserve_metadata(&review.labels, &stripped).is_err());
        assert!(ensure_provider(&conn, &review.labels, Some(&reviewer), None).is_ok());

        conn.execute(
            "UPDATE task_target_presence SET report_seen_at=?1",
            params![now - 90_000],
        )
        .unwrap();
        update(&conn, &review.id, "in_progress", &peer).unwrap();
        // A later opposite-provider registration doesn't invalidate work that
        // was already validly claimed under fallback.
        conn.execute(
            "UPDATE task_target_presence SET report_seen_at=?1",
            params![db::now_ms()],
        )
        .unwrap();
        update(&conn, &review.id, "in_progress", &peer).unwrap();
        update(&conn, &review.id, "done", &peer).unwrap();
    }

    #[test]
    fn review_uses_contributor_provider_when_the_source_has_no_owner() {
        let (database, source, _) = fixture();
        let conn = database.lock().unwrap();
        let mut worker = actor();
        worker.provider = "claude".into();
        task_events::mutation(&conn, Some(&worker), |conn| {
            db::insert_comment(
                conn,
                &source.id,
                "worker",
                "agent",
                "implemented and verified",
            )
        })
        .unwrap();
        conn.execute(
            "UPDATE tasks SET owner_agent_id=NULL,owner_origin_json=NULL WHERE id=?1",
            params![source.id],
        )
        .unwrap();
        let review =
            task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &source.id))
                .unwrap()
                .unwrap();
        assert!(review.labels.contains(&"review-provider:codex".into()));
    }

    #[test]
    fn provider_policy_is_symmetric_and_counts_rest_only_registrations() {
        let (database, _, _) = fixture();
        let conn = database.lock().unwrap();
        let codex = actor();
        let mut claude = actor();
        claude.provider = "claude".into();
        let labels = vec!["task-review".into(), "review-provider:codex".into()];
        assert!(ensure_provider(&conn, &labels, Some(&claude), None).is_ok());
        conn.execute(
            "INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,advertised)
             VALUES('example',?1,?2,?3,'fixture',?4,1)",
            params![codex.instance_id, codex.session_id, serde_json::to_string(&codex).unwrap(), db::now_ms()],
        ).unwrap();
        assert!(ensure_provider(&conn, &labels, Some(&claude), None).is_err());
        assert!(ensure_provider(&conn, &labels, Some(&codex), None).is_ok());
        conn.execute(
            "UPDATE task_target_presence SET seen_at=?1",
            params![db::now_ms() - crate::task_targets::TTL_MS - 1],
        )
        .unwrap();
        assert!(ensure_provider(&conn, &labels, Some(&claude), None).is_ok());
    }

    #[test]
    fn agents_that_commented_before_a_handoff_are_also_excluded() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES ('example','none','',0)", []).unwrap();
        let (worker, finisher) = (actor(), actor());
        let source = task_events::mutation(&conn, Some(&worker), |conn| {
            db::insert_task(conn, "example", "Handed off", None, None, &[], None, "w")
        })
        .unwrap();
        update(&conn, &source.id, "in_progress", &worker).unwrap();
        task_events::mutation(&conn, Some(&worker), |conn| {
            db::insert_comment(conn, &source.id, "worker", "agent", "pushed the fix")
        })
        .unwrap();
        update(&conn, &source.id, "todo", &worker).unwrap();
        update(&conn, &source.id, "in_progress", &finisher).unwrap();
        update(&conn, &source.id, "done", &finisher).unwrap();
        let review =
            task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &source.id))
                .unwrap()
                .unwrap();
        for agent in [&worker, &finisher] {
            let label = format!("review-exclude:{}", agent.session_id);
            assert_eq!(review.labels.iter().filter(|l| **l == label).count(), 1);
            assert!(update(&conn, &review.id, "in_progress", agent).is_err());
        }
    }
    #[test]
    fn concurrent_hosts_create_one_review_and_rollback_leaves_no_orphan() {
        let (database, source, _) = fixture();
        {
            let conn = database.lock().unwrap();
            let result: Result<()> = task_events::mutation(&conn, None, |conn| {
                enqueue(conn, "example", &source.id)?;
                anyhow::bail!("simulate transaction failure")
            });
            assert!(result.is_err());
            assert_eq!(
                conn.query_row("SELECT COUNT(*) FROM tasks", [], |row| row.get::<_, i64>(0))
                    .unwrap(),
                1
            );
        }
        let handles: Vec<_> = (0..8)
            .map(|_| {
                let database = database.clone();
                let id = source.id.clone();
                std::thread::spawn(move || {
                    let conn = database.lock().unwrap();
                    task_events::mutation(&conn, None, |conn| enqueue(conn, "example", &id))
                        .unwrap()
                        .unwrap()
                        .id
                })
            })
            .collect();
        let ids: Vec<_> = handles
            .into_iter()
            .map(|handle| handle.join().unwrap())
            .collect();
        assert!(ids.iter().all(|id| id == &ids[0]));
    }
}
