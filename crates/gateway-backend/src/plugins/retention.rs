//! Message and router bookkeeping retention, driven by the shared Clock.
//! Cleanup runs at startup and then daily; failures remain router deliveries.
use crate::{
    db,
    plugins::{clock, remote},
    router::{self, Event, EventDefinition, Plugin, Step, StepContext, StepOutcome, Subscription},
};
use anyhow::Result;
use async_trait::async_trait;
use rusqlite::{params, Connection, OptionalExtension};

pub const STARTUP: &str = "retention.startup";
const DAY_MS: i64 = 86_400_000;

pub fn definition() -> EventDefinition {
    EventDefinition {
        id: STARTUP,
        version: 1,
        remote_emit: false,
        validate: router::object,
    }
}

pub fn initialize(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS retention_runs (
            id INTEGER PRIMARY KEY CHECK(id=1),
            completed_at INTEGER NOT NULL
        );",
    )?;
    Ok(())
}

#[derive(Clone, Copy)]
pub struct Retention {
    retention_ms: i64,
    bot_retention_ms: i64,
}

impl Retention {
    pub fn new(retention_days: u64, bot_retention_days: u64) -> Self {
        Self {
            retention_ms: i64::try_from(retention_days)
                .unwrap_or(i64::MAX)
                .saturating_mul(DAY_MS),
            bot_retention_ms: i64::try_from(bot_retention_days)
                .unwrap_or(i64::MAX)
                .saturating_mul(DAY_MS),
        }
    }
}

impl Plugin for Retention {
    fn id(&self) -> &'static str {
        "retention"
    }

    fn subscriptions(&self) -> Vec<Subscription> {
        [STARTUP, clock::TICK]
            .into_iter()
            .map(|event| Subscription {
                event,
                versions: 1..=1,
                depends_on: Vec::new(),
            })
            .collect()
    }

    fn exec(&self) -> Vec<Box<dyn Step>> {
        vec![Box::new(*self)]
    }
}

#[async_trait]
impl Step for Retention {
    async fn run(&self, event: &Event, ctx: &StepContext) -> StepOutcome {
        let database = ctx.db().clone();
        let retention = *self;
        let now = event.emitted_at;
        let startup = event.id == STARTUP;
        let result = tokio::task::spawn_blocking(move || {
            let conn = database.lock().unwrap();
            retention.clean(&conn, now, startup)
        })
        .await
        .unwrap_or_else(|error| Err(error.into()));
        match result {
            Ok(count) => {
                if let Some(count) = count {
                    tracing::info!("Purged {count} old messages");
                }
                StepOutcome::Continue
            }
            Err(error) => {
                tracing::warn!("Purge error: {error}");
                StepOutcome::Fail(error.to_string())
            }
        }
    }
}

impl Retention {
    fn clean(&self, conn: &Connection, now: i64, startup: bool) -> Result<Option<usize>> {
        let tx = conn.unchecked_transaction()?;
        let previous: Option<i64> = tx
            .query_row(
                "SELECT completed_at FROM retention_runs WHERE id=1",
                [],
                |row| row.get(0),
            )
            .optional()?;
        if previous.is_some_and(|previous| now <= previous || (!startup && now - previous < DAY_MS))
        {
            return Ok(None);
        }
        // Open controller deliveries and outstanding remote cursors survive
        // cleanup. Remote owns its replay boundary; the router stays generic.
        router::prune(
            &tx,
            now.saturating_sub(DAY_MS),
            remote::retention_cursor(&tx, now)?,
        )?;
        let count = db::purge_old_messages_with_policy(
            &tx,
            now.saturating_sub(self.retention_ms),
            now.saturating_sub(self.bot_retention_ms),
        )?;
        tx.execute(
            "INSERT INTO retention_runs(id,completed_at) VALUES(1,?1)
             ON CONFLICT(id) DO UPDATE SET completed_at=excluded.completed_at",
            params![now],
        )?;
        tx.commit()?;
        Ok(Some(count))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn seed(conn: &Connection, now: i64) {
        conn.execute("INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('p','discord','',0)", []).unwrap();
        for (source, author, age) in [
            ("agent", None, 3 * DAY_MS),
            ("user", Some("bot"), 2 * DAY_MS),
            ("agent", None, DAY_MS),
            ("user", None, 3 * DAY_MS),
        ] {
            conn.execute(
                "INSERT INTO messages(project_ident,source,content,sent_at,author_kind) VALUES('p',?1,'message',?2,?3)",
                params![source, now - age, author],
            ).unwrap();
        }
    }

    #[test]
    fn cleanup_keeps_retention_policy_and_runs_at_startup_then_daily() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        let now = 10 * DAY_MS;
        seed(&conn, now);
        let retention = Retention::new(2, 1);
        assert_eq!(retention.clean(&conn, now, true).unwrap(), Some(2));
        assert_eq!(
            retention.clean(&conn, now, true).unwrap(),
            None,
            "replayed startup is idempotent"
        );
        assert_eq!(
            retention.clean(&conn, now + DAY_MS - 1, false).unwrap(),
            None
        );
        assert_eq!(
            retention.clean(&conn, now + DAY_MS, false).unwrap(),
            Some(0)
        );
        assert_eq!(
            retention.clean(&conn, now + DAY_MS + 1, true).unwrap(),
            Some(1),
            "a fresh startup still cleans immediately"
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM messages", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            1,
            "unacknowledged human messages survive"
        );
    }

    #[test]
    fn cleanup_preserves_remote_replay_and_prunes_normally_without_consumers() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        let retention = Retention::new(30, 7);
        for seq in 1..=2 {
            conn.execute("INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at) VALUES(?1,?2,'task.created',1,'{}','fixture',0)",params![seq,uuid::Uuid::now_v7().to_string()]).unwrap();
            conn.execute("INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at) VALUES(?1,'remote','completed',0,0)",params![seq]).unwrap();
        }
        assert_eq!(remote::retention_cursor(&conn, 2 * DAY_MS).unwrap(), None);
        retention.clean(&conn, 2 * DAY_MS, true).unwrap();
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM router_events", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0,
            "normal bookkeeping cleanup has no external boundary"
        );
        conn.execute("INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at) VALUES(3,?1,'task.created',1,'{}','fixture',0)",params![uuid::Uuid::now_v7().to_string()]).unwrap();
        conn.execute("INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at) VALUES(3,'remote','completed',0,0)",[]).unwrap();
        conn.execute("INSERT INTO remote_consumers(consumer_id,connection_id,cursor,last_seen_at) VALUES('offline',NULL,2,?1)",params![3 * DAY_MS]).unwrap();
        retention.clean(&conn, 3 * DAY_MS, true).unwrap();
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM router_events", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            1,
            "offline outstanding delivery survives actual retention cleanup"
        );
        conn.execute("UPDATE remote_consumers SET cursor=3", [])
            .unwrap();
        retention.clean(&conn, 4 * DAY_MS, true).unwrap();
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM router_events", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0,
            "acknowledged remote delivery can be pruned"
        );
    }

    #[test]
    fn cleanup_expires_orphaned_consumers_but_preserves_recent_offline_replay() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        let now = 10 * DAY_MS;
        for seq in 1..=3 {
            conn.execute("INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at) VALUES(?1,?2,'task.created',1,'{}','fixture',0)",params![seq,uuid::Uuid::now_v7().to_string()]).unwrap();
            conn.execute("INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at) VALUES(?1,'remote',?2,0,0)",params![seq,if seq == 3 {"retrying"} else {"completed"}]).unwrap();
        }
        // A crash can leave connection_id set; time, not that stale marker,
        // determines whether the consumer still holds a replay obligation.
        conn.execute(
            "INSERT INTO remote_consumers VALUES('orphan','dead-connection',0,?1)",
            params![now - DAY_MS],
        )
        .unwrap();
        conn.execute(
            "INSERT INTO remote_consumers VALUES('recent',NULL,1,?1)",
            params![now - 1],
        )
        .unwrap();
        let retention = Retention::new(30, 7);
        retention.clean(&conn, now, true).unwrap();
        let sequences = || {
            conn.prepare("SELECT seq FROM router_events ORDER BY seq")
                .unwrap()
                .query_map([], |row| row.get::<_, i64>(0))
                .unwrap()
                .collect::<rusqlite::Result<Vec<_>>>()
                .unwrap()
        };
        assert_eq!(
            sequences(),
            [2, 3],
            "recent offline cursor protects its unread range"
        );
        let consumers: Vec<String> = conn
            .prepare("SELECT consumer_id FROM remote_consumers")
            .unwrap()
            .query_map([], |row| row.get(0))
            .unwrap()
            .collect::<rusqlite::Result<_>>()
            .unwrap();
        assert_eq!(consumers, ["recent"]);
        retention.clean(&conn, now + DAY_MS, false).unwrap();
        assert_eq!(sequences(), [3], "expired consumers cannot block settled event cleanup; unfinished controller work survives");
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM remote_consumers", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0
        );
    }

    #[test]
    fn failed_cleanup_rolls_back_deletions_and_can_retry_the_same_event() {
        let database = db::open(":memory:").unwrap();
        let conn = database.lock().unwrap();
        let now = 10 * DAY_MS;
        seed(&conn, now);
        conn.execute_batch(
            "CREATE TRIGGER fail_retention BEFORE INSERT ON retention_runs
             BEGIN SELECT RAISE(ABORT,'injected retention failure'); END;",
        )
        .unwrap();
        let retention = Retention::new(2, 1);
        assert!(retention.clean(&conn, now, true).is_err());
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM messages", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            4
        );
        assert_eq!(
            conn.query_row("SELECT COUNT(*) FROM retention_runs", [], |row| row
                .get::<_, i64>(0))
                .unwrap(),
            0
        );
        conn.execute_batch("DROP TRIGGER fail_retention").unwrap();
        assert_eq!(retention.clean(&conn, now, true).unwrap(), Some(2));
    }
}
