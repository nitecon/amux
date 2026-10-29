//! Internal channel ingestion. Transport input is durable once accepted by the
//! bus; message insertion and backfill cursor advancement then retry together.
//! Input is globally FIFO, as the original channel processor was: a retry holds
//! later channel input across projects, while independent controllers progress.
use crate::{
    db,
    router::{Event, EventDefinition, Plugin, Step, StepContext, StepOutcome, Subscription},
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection, OptionalExtension};
use serde::Deserialize;

pub const RECEIVED: &str = "channel.message_received";

#[derive(Deserialize)]
struct Received {
    channel_name: String,
    room_id: String,
    message: Message,
}

#[derive(Deserialize)]
struct Message {
    id: String,
    content: String,
    author_kind: String,
}

pub fn definition() -> EventDefinition {
    EventDefinition {
        id: RECEIVED,
        version: 1,
        remote_emit: false,
        validate: |payload| {
            serde_json::from_value::<Received>(payload.clone())
                .map(|_| ())
                .map_err(|error| error.to_string())
        },
    }
}

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS inbound_cursors (
            project TEXT PRIMARY KEY REFERENCES projects(ident) ON DELETE CASCADE,
            event_seq INTEGER NOT NULL
        );",
    )?;
    Ok(())
}

pub struct Inbound;

impl Plugin for Inbound {
    fn id(&self) -> &'static str {
        "inbound"
    }

    fn subscriptions(&self) -> Vec<Subscription> {
        vec![Subscription {
            event: RECEIVED,
            versions: 1..=1,
            depends_on: Vec::new(),
        }]
    }

    fn fifo(&self) -> bool {
        true
    }

    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(Self)]
    }
}

#[async_trait]
impl Step for Inbound {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let database = ctx.db().clone();
        let event = event.clone();
        match tokio::task::spawn_blocking(move || {
            let conn = database.lock().unwrap();
            ingest(&conn, &event)
        })
        .await
        .unwrap_or_else(|error| Err(error.into()))
        {
            Ok(outcome) => outcome,
            Err(error) => StepOutcome::Fail(error.to_string()),
        }
    }
}

fn ingest(conn: &Connection, event: &Event) -> Result<StepOutcome> {
    let received: Received = serde_json::from_value(event.payload.clone())?;
    let tx = conn.unchecked_transaction()?;
    let Some(project) = db::get_project_by_room(&tx, &received.channel_name, &received.room_id)?
    else {
        tracing::warn!(
            "Received message for unknown room {} on {}",
            received.room_id,
            received.channel_name
        );
        return Ok(StepOutcome::Stop);
    };
    let message = received.message;
    // Provider redelivery has a new router sequence. It must neither insert a
    // second message nor move the backfill cursor to an older external id.
    if db::message_external_id_exists(&tx, &project.ident, &message.id)? {
        return Ok(StepOutcome::Continue);
    }
    db::insert_message(
        &tx,
        &db::Message {
            id: 0,
            project_ident: project.ident.clone(),
            source: "user".into(),
            external_message_id: Some(message.id.clone()),
            content: message.content,
            sent_at: event.emitted_at,
            confirmed_at: None,
            parent_message_id: None,
            agent_id: None,
            message_type: "message".into(),
            subject: None,
            hostname: None,
            event_at: None,
            deliver_to_agents: false,
            author_kind: Some(message.author_kind),
            resolved_at: None,
            resolved_by: None,
        },
    )?;
    let latest: Option<i64> = tx
        .query_row(
            "SELECT event_seq FROM inbound_cursors WHERE project=?1",
            [&project.ident],
            |row| row.get(0),
        )
        .optional()?;
    // Keep the store defensive against an older direct replay. The router's
    // FIFO policy enforces source order before messages become visible.
    if latest.is_none_or(|latest| event.seq > latest) {
        db::update_last_msg_id(&tx, &project.ident, &message.id)?;
        tx.execute(
            "INSERT INTO inbound_cursors(project,event_seq) VALUES(?1,?2)
             ON CONFLICT(project) DO UPDATE SET event_seq=excluded.event_seq",
            params![project.ident, event.seq],
        )?;
    }
    tx.commit()?;
    Ok(StepOutcome::Continue)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::router;
    use serde_json::json;
    use std::time::Duration;

    fn project(conn: &Connection) {
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','room',0)", []).unwrap();
    }

    fn event(seq: i64, external: &str) -> Event {
        Event {
            seq,
            id: RECEIVED.into(),
            version: 1,
            uid: uuid::Uuid::now_v7(),
            payload: json!({"channel_name":"discord","room_id":"room","message":{"id":external,"content":"hello","author_kind":"human"}}),
            source: "channel:discord".into(),
            emitted_at: 1000 + seq,
            correlation: None,
        }
    }

    fn messages(conn: &Connection) -> i64 {
        conn.query_row("SELECT COUNT(*) FROM messages", [], |row| row.get(0))
            .unwrap()
    }

    fn cursor(conn: &Connection) -> Option<String> {
        conn.query_row(
            "SELECT last_msg_id FROM projects WHERE ident='p'",
            [],
            |row| row.get(0),
        )
        .unwrap()
    }

    fn fail_cursor(conn: &Connection) {
        conn.execute_batch(
            "CREATE TRIGGER fail_inbound_cursor BEFORE UPDATE OF last_msg_id ON projects
             BEGIN SELECT RAISE(ABORT,'injected cursor failure'); END;",
        )
        .unwrap();
    }

    #[test]
    fn cursor_failure_rolls_back_message_and_replay_repairs_both_without_duplicates() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        project(&conn);
        fail_cursor(&conn);
        let received = event(1, "first");
        assert!(ingest(&conn, &received).is_err());
        assert_eq!(messages(&conn), 0);
        assert_eq!(cursor(&conn), None);
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM inbound_cursors", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0
        );
        conn.execute_batch("DROP TRIGGER fail_inbound_cursor")
            .unwrap();
        assert_eq!(ingest(&conn, &received).unwrap(), StepOutcome::Continue);
        assert_eq!(messages(&conn), 1);
        assert_eq!(cursor(&conn).as_deref(), Some("first"));
        assert_eq!(ingest(&conn, &received).unwrap(), StepOutcome::Continue);
        assert_eq!(messages(&conn), 1);
    }

    #[test]
    fn direct_store_replays_and_provider_redelivery_never_rewind_the_cursor() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        project(&conn);
        ingest(&conn, &event(2, "newest")).unwrap();
        ingest(&conn, &event(1, "older")).unwrap();
        assert_eq!(
            messages(&conn),
            2,
            "older pending messages are still ingested"
        );
        assert_eq!(cursor(&conn).as_deref(), Some("newest"));
        ingest(&conn, &event(3, "older")).unwrap();
        assert_eq!(
            messages(&conn),
            2,
            "provider redelivery has no second message"
        );
        assert_eq!(cursor(&conn).as_deref(), Some("newest"));
        let latest: i64 = conn
            .query_row(
                "SELECT event_seq FROM inbound_cursors WHERE project='p'",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert_eq!(latest, 2, "redelivery does not replace the checkpoint");
    }

    #[tokio::test]
    async fn accepted_input_is_durable_and_retries_failed_ingestion() {
        let database = db::open(":memory:").unwrap();
        {
            let conn = database.lock().unwrap();
            project(&conn);
            fail_cursor(&conn);
        }
        let (bus, worker) = router::start(
            database.clone(),
            vec![definition()],
            vec![Box::new(Inbound)],
        )
        .unwrap();
        bus.emit(
            "channel:discord",
            RECEIVED,
            1,
            event(1, "first").payload,
            None,
        )
        .await
        .unwrap();
        let state = || {
            database
                .lock()
                .unwrap()
                .query_row(
                    "SELECT state FROM router_deliveries WHERE plugin='inbound'",
                    [],
                    |row| row.get::<_, String>(0),
                )
                .unwrap()
        };
        tokio::time::timeout(Duration::from_secs(5), async {
            while state() != "retrying" {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        {
            let conn = database.lock().unwrap();
            assert_eq!(messages(&conn), 0);
            conn.execute_batch("DROP TRIGGER fail_inbound_cursor; UPDATE router_deliveries SET next_attempt_at=0 WHERE plugin='inbound';").unwrap();
        }
        bus.wake();
        tokio::time::timeout(Duration::from_secs(5), async {
            while state() != "completed" {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        let conn = database.lock().unwrap();
        assert_eq!(messages(&conn), 1);
        assert_eq!(cursor(&conn).as_deref(), Some("first"));
        worker.abort();
    }

    #[tokio::test]
    async fn fifo_retries_hide_later_input_until_source_order_can_commit() {
        use std::sync::{
            atomic::{AtomicUsize, Ordering},
            Arc,
        };
        struct Independent(Arc<AtomicUsize>);
        impl Plugin for Independent {
            fn id(&self) -> &'static str {
                "independent"
            }
            fn subscriptions(&self) -> Vec<Subscription> {
                vec![Subscription {
                    event: RECEIVED,
                    versions: 1..=1,
                    depends_on: Vec::new(),
                }]
            }
            fn exec(&self) -> Vec<Box<dyn Step>> {
                vec![Box::new(Self(self.0.clone()))]
            }
        }
        #[async_trait]
        impl Step for Independent {
            async fn run(&self, _: &Event, _: &StepContext) -> StepOutcome {
                self.0.fetch_add(1, Ordering::SeqCst);
                StepOutcome::Continue
            }
        }
        let database = db::open(":memory:").unwrap();
        {
            let conn = database.lock().unwrap();
            project(&conn);
            conn.execute_batch(
                "CREATE TRIGGER fail_first_input BEFORE UPDATE OF last_msg_id ON projects
                 WHEN NEW.last_msg_id='first' BEGIN SELECT RAISE(ABORT,'first input failed'); END;",
            )
            .unwrap();
        }
        let progress = Arc::new(AtomicUsize::new(0));
        let (bus, worker) = router::start(
            database.clone(),
            vec![definition()],
            vec![Box::new(Inbound), Box::new(Independent(progress.clone()))],
        )
        .unwrap();
        let first = bus
            .emit(
                "channel:discord",
                RECEIVED,
                1,
                event(1, "first").payload,
                None,
            )
            .await
            .unwrap();
        let state = |uid: uuid::Uuid| -> String {
            database.lock().unwrap().query_row(
                "SELECT d.state FROM router_deliveries d JOIN router_events e ON e.seq=d.event_seq
                 WHERE e.uid=?1 AND d.plugin='inbound'", params![uid.to_string()], |row| row.get(0),
            ).unwrap()
        };
        tokio::time::timeout(Duration::from_secs(5), async {
            while state(first) != "retrying" {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        let second = bus
            .emit(
                "channel:discord",
                RECEIVED,
                1,
                event(2, "second").payload,
                None,
            )
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            while progress.load(Ordering::SeqCst) != 2 {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        assert_eq!(
            state(second),
            "pending",
            "the second input cannot overtake a retry"
        );
        {
            let conn = database.lock().unwrap();
            assert_eq!(messages(&conn), 0);
            assert!(db::get_unconfirmed_for_agent(&conn, "p", "reader")
                .unwrap()
                .is_empty());
            assert_eq!(cursor(&conn), None);
            // Outgoing writes keep their existing commit-order IDs; only the
            // source's incoming stream is FIFO, not all application mutations.
            conn.execute("INSERT INTO messages(project_ident,source,content,sent_at) VALUES('p','agent','outgoing',1)", []).unwrap();
            conn.execute_batch("DROP TRIGGER fail_first_input;
                UPDATE router_deliveries SET next_attempt_at=0 WHERE plugin='inbound' AND state='retrying';").unwrap();
        }
        bus.wake();
        tokio::time::timeout(Duration::from_secs(5), async {
            while state(second) != "completed" {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
        let conn = database.lock().unwrap();
        let unread = db::get_unconfirmed_for_agent(&conn, "p", "reader").unwrap();
        assert_eq!(
            unread
                .iter()
                .map(|message| message.external_message_id.as_deref())
                .collect::<Vec<_>>(),
            [Some("first"), Some("second")]
        );
        assert!(unread[0].id < unread[1].id);
        assert_eq!(
            unread[0].id, 2,
            "the outgoing message keeps its earlier commit ID"
        );
        assert_eq!(messages(&conn), 3);
        assert_eq!(cursor(&conn).as_deref(), Some("second"));
        worker.abort();
    }
}
