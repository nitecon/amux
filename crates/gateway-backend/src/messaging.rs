//! Native message storage and optional external delivery. Internal receipts name
//! committed database rows; they never claim an external transport accepted a send.
use crate::{channel::OutboundMessage, db, routes::AppError, AppState};
use axum::http::StatusCode;

type Result<T> = std::result::Result<T, AppError>;

pub const INTERNAL: &str = "internal";

/// Native rooms are project identities. External rooms are provisioned only
/// when an operator explicitly chooses a configured integration.
pub async fn room(state: &AppState, ident: &str, channel: &str, room: String) -> Result<String> {
    if channel == INTERNAL {
        return Ok(ident.to_owned());
    }
    let plugin = state.plugins.get(channel).ok_or_else(|| {
        AppError(
            StatusCode::SERVICE_UNAVAILABLE,
            format!("channel plugin '{channel}' is not configured"),
        )
    })?;
    external_room(state, ident, plugin.as_ref(), room).await
}

/// Resolve the plugin room for an outbound send, creating it on first use for
/// adhoc projects that were registered without one.
pub(crate) async fn external_room(
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
    tokio::task::spawn_blocking(move || {
        let conn = db.lock().unwrap();
        db::set_project_room(&conn, &ident, &persisted)
    })
    .await??;
    Ok(created)
}

/// None means native storage is the destination, not successful external delivery.
pub async fn send(
    state: &AppState,
    ident: &str,
    channel: &str,
    room_id: String,
    parent: Option<&str>,
    message: &OutboundMessage,
) -> Result<Option<String>> {
    if channel == INTERNAL {
        return Ok(None);
    }
    let room_id = room(state, ident, channel, room_id).await?;
    let plugin = &state.plugins[channel];
    Ok(Some(match parent {
        Some(parent) => plugin.reply_structured(&room_id, parent, message).await?,
        None => plugin.send_structured(&room_id, message).await?,
    }))
}

/// Agent registration, the message, sender confirmation and native receipt are
/// one transaction. A storage error cannot leave a receipt pointing at no row.
pub fn store(conn: &rusqlite::Connection, message: &db::Message) -> anyhow::Result<(i64, String)> {
    let tx = conn.unchecked_transaction()?;
    if let Some(agent) = message.agent_id.as_deref() {
        db::upsert_agent(&tx, &message.project_ident, agent)?;
    }
    let id = db::insert_message(&tx, message)?;
    let receipt = match &message.external_message_id {
        Some(external) => external.clone(),
        None => {
            let receipt = format!("internal:{id}");
            tx.execute(
                "UPDATE messages SET external_message_id=?1 WHERE id=?2",
                rusqlite::params![receipt, id],
            )?;
            receipt
        }
    };
    tx.commit()?;
    Ok((id, receipt))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_receipt_survives_reopen_and_failed_receipt_rolls_back_every_write() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("messages.sqlite");
        let database = db::open(path.to_str().unwrap()).unwrap();
        let (id, receipt) = {
            let conn = database.lock().unwrap();
            conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','internal','p',0)", []).unwrap();
            let message = db::Message {
                id: 0,
                project_ident: "p".into(),
                source: "agent".into(),
                external_message_id: None,
                content: "persist me".into(),
                sent_at: 10,
                confirmed_at: None,
                parent_message_id: None,
                agent_id: Some("writer".into()),
                message_type: "message".into(),
                subject: None,
                hostname: None,
                event_at: None,
                deliver_to_agents: false,
                author_kind: Some("agent".into()),
                resolved_at: None,
                resolved_by: None,
            };
            conn.execute_batch("CREATE TRIGGER fail_receipt BEFORE UPDATE OF external_message_id ON messages BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END;").unwrap();
            assert!(store(&conn, &message).is_err());
            for table in ["messages", "agents", "agent_confirmations"] {
                assert_eq!(
                    conn.query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |r| r
                        .get::<_, i64>(0))
                        .unwrap(),
                    0
                );
            }
            conn.execute_batch("DROP TRIGGER fail_receipt").unwrap();
            store(&conn, &message).unwrap()
        };
        drop(database);
        let reopened = db::open(path.to_str().unwrap()).unwrap();
        let conn = reopened.lock().unwrap();
        let stored = db::get_message_by_id(&conn, "p", id).unwrap().unwrap();
        assert_eq!(stored.external_message_id, Some(receipt));
        assert_eq!(stored.content, "persist me");
        assert_eq!(stored.agent_id.as_deref(), Some("writer"));
        assert_eq!(
            conn.query_row(
                "SELECT COUNT(*) FROM agent_confirmations WHERE message_id=?1 AND agent_id='writer'",
                [id],
                |r| r.get::<_, i64>(0)
            )
            .unwrap(),
            1
        );
    }
}
