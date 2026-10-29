use amux::migration;
use amux_gateway_backend::db;
use rusqlite::Connection;
use std::{fs, path::Path};

fn fixture() -> tempfile::TempDir {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&root).unwrap();
    tempfile::tempdir_in(root).unwrap()
}

fn source(path: &Path) -> db::Db {
    let database = db::open(path.to_str().unwrap()).unwrap();
    database.lock().unwrap().execute_batch(
        "PRAGMA wal_autocheckpoint=0;
         INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('demo','discord','original-room',1);
         INSERT INTO tasks(id,project_ident,title,reporter,created_at,updated_at) VALUES('original-task','demo','Original task','fixture',1,1);
         INSERT INTO task_comments(id,task_id,author,author_type,content,created_at) VALUES('original-comment','original-task','fixture','user','Keep full history',2);
         INSERT INTO gateway_memories(id,project_ident,server_revision,content,memory_type,content_hash,created_at,updated_at) VALUES('original-memory','demo',1,'Keep the original memory','project','hash',1,1);
         CREATE TABLE \"future records\"(id TEXT PRIMARY KEY, body BLOB);
         CREATE INDEX future_record_index ON \"future records\"(body);
         INSERT INTO \"future records\" VALUES('original-id',X'000102FF');"
    ).unwrap();
    database
}

#[test]
fn complete_wal_snapshot_preserves_ids_history_unknown_tables_indexes_and_source() {
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let original = source(&path);
    assert!(
        fs::metadata(path.with_extension("sqlite-wal"))
            .unwrap()
            .len()
            > 32
    );
    let state = fixture.path().join("amux");
    let preview = migration::prepare_local(&path, &state).unwrap();
    assert_eq!(preview.inventory.tables["tasks"], 1);
    assert_eq!(preview.inventory.tables["task_comments"], 1);
    assert_eq!(preview.inventory.tables["gateway_memories"], 1);
    // A reviewed snapshot stays immutable while the legacy server accepts writes.
    original.lock().unwrap().execute("INSERT INTO task_comments(id,task_id,author,author_type,content,created_at) VALUES('later','original-task','fixture','user','After preview',3)", []).unwrap();
    let installed = migration::install(&preview, &state).unwrap();
    assert!(installed.receipt.is_file());
    let imported = Connection::open(&installed.database).unwrap();
    let room: String = imported
        .query_row("SELECT room_id FROM projects WHERE ident='demo'", [], |r| {
            r.get(0)
        })
        .unwrap();
    assert_eq!(room, "original-room");
    let id: String = imported
        .query_row("SELECT id FROM tasks", [], |r| r.get(0))
        .unwrap();
    assert_eq!(id, "original-task");
    let comment: String = imported
        .query_row("SELECT content FROM task_comments", [], |r| r.get(0))
        .unwrap();
    assert_eq!(comment, "Keep full history");
    let memory: String = imported
        .query_row("SELECT content FROM gateway_memories", [], |r| r.get(0))
        .unwrap();
    assert_eq!(memory, "Keep the original memory");
    let body: Vec<u8> = imported
        .query_row(
            "SELECT body FROM \"future records\" WHERE id='original-id'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(body, vec![0, 1, 2, 255]);
    assert!(
        imported
            .prepare("SELECT 1 FROM sqlite_master WHERE name='future_record_index'")
            .unwrap()
            .exists([])
            .unwrap()
    );
    assert_eq!(
        original
            .lock()
            .unwrap()
            .query_row("SELECT count(*) FROM task_comments", [], |r| r
                .get::<_, i64>(0))
            .unwrap(),
        2
    );
    assert!(
        !state.join("gateway-preferences.json").exists(),
        "Import must not switch the connection"
    );
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        assert_eq!(
            fs::metadata(installed.database)
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o600
        );
    }
}

#[test]
fn retry_and_final_snapshot_refresh_are_safe_but_local_edits_are_never_overwritten() {
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let original = source(&path);
    let state = fixture.path().join("amux");
    let first = migration::prepare_local(&path, &state).unwrap();
    let installed = migration::install(&first, &state).unwrap();
    migration::install(&first, &state).unwrap();
    original
        .lock()
        .unwrap()
        .execute("UPDATE tasks SET title='Final gateway snapshot'", [])
        .unwrap();
    let final_snapshot = migration::prepare_local(&path, &state).unwrap();
    migration::install(&final_snapshot, &state).unwrap();
    assert!(fs::read_dir(state.join("server")).unwrap().any(|entry| {
        entry
            .unwrap()
            .file_name()
            .to_string_lossy()
            .starts_with("before-migration-")
    }));
    let local = Connection::open(&installed.database).unwrap();
    local.execute_batch("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; UPDATE tasks SET title='Local work'").unwrap();
    let error = migration::install(&first, &state).unwrap_err().to_string();
    assert!(error.contains("already contains data"));
    assert_eq!(
        local
            .query_row("SELECT title FROM tasks", [], |r| r.get::<_, String>(0))
            .unwrap(),
        "Local work"
    );
}

#[test]
fn active_server_populated_target_invalid_source_and_changed_preview_are_rejected() {
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let _source = source(&path);
    let state = fixture.path().join("amux");
    let mut preview = migration::prepare_local(&path, &state).unwrap();
    let target = state.join("server/application.sqlite");
    let lock = fs::File::create(state.join("server/application.sqlite.amux-server.lock")).unwrap();
    lock.try_lock().unwrap();
    assert!(migration::install(&preview, &state).is_err());
    assert!(!target.exists());
    drop(lock);
    preview.inventory.sha256 = "changed".into();
    assert!(
        migration::install(&preview, &state)
            .unwrap_err()
            .to_string()
            .contains("changed")
    );
    let _existing = source(&target);
    let preview = migration::prepare_local(&path, &state).unwrap();
    assert!(
        migration::install(&preview, &state)
            .unwrap_err()
            .to_string()
            .contains("already contains data")
    );
    assert!(
        migration::prepare_local(&target, &state)
            .unwrap_err()
            .to_string()
            .contains("already this server")
    );
    let invalid = fixture.path().join("invalid.sqlite");
    fs::write(&invalid, b"not sqlite").unwrap();
    assert!(migration::prepare_local(&invalid, &state).is_err());
    let broken = Connection::open(&path).unwrap();
    broken.execute_batch("PRAGMA foreign_keys=OFF; INSERT INTO messages(project_ident,source,content,sent_at) VALUES('missing-project','user','invalid foreign key',1)").unwrap();
    assert!(
        migration::prepare_local(&path, &state)
            .unwrap_err()
            .to_string()
            .contains("broken foreign keys")
    );
}

#[test]
fn unused_existing_database_is_backed_up_before_install() {
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let _source = source(&path);
    let state = fixture.path().join("amux");
    let preview = migration::prepare_local(&path, &state).unwrap();
    let empty = db::open(state.join("server/application.sqlite").to_str().unwrap()).unwrap();
    empty
        .lock()
        .unwrap()
        .execute_batch(
            "UPDATE presence_clock SET observed_at=12345;
         INSERT INTO retention_runs(id,completed_at) VALUES(1,12345);",
        )
        .unwrap();
    drop(empty);
    migration::install(&preview, &state).unwrap();
    assert!(fs::read_dir(state.join("server")).unwrap().any(|entry| {
        entry
            .unwrap()
            .file_name()
            .to_string_lossy()
            .starts_with("before-migration-")
    }));
}

#[test]
fn controller_work_identity_reports_settings_and_unknown_rows_prevent_overwrite() {
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let _source = source(&path);
    let preview = migration::prepare_local(&path, &fixture.path().join("preview")).unwrap();
    for (name, sql, table) in [
        ("pending-event", "INSERT INTO router_events(uid,id,version,payload,source,emitted_at) VALUES('pending','channel.message_received',1,'{}','channel:discord',1);
            INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at) VALUES(1,'inbound','pending',1,1);", "router_deliveries"),
        ("task-input", "INSERT INTO router_task_outbox(kind,project,task_json,created_at) VALUES('created','p','{}',1);", "router_task_outbox"),
        ("observed-agent", "INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,advertised,source,agent_json,report_seen_at) VALUES('p','instance','observed','{}','g',0,'client:node','{}',1);", "task_target_presence"),
        ("advertised-agent", "INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation) VALUES('p','instance','advertised','{}','g');", "task_target_presence"),
        ("settings", "INSERT INTO settings(key,value) VALUES('plugin.assignment.enabled','false');", "settings"),
        ("unknown", "CREATE TABLE operator_data(value TEXT); INSERT INTO operator_data VALUES('keep');", "operator_data"),
    ] {
        let state = fixture.path().join(name);
        fs::create_dir_all(state.join("server")).unwrap();
        let target = state.join("server/application.sqlite");
        let database = db::open(target.to_str().unwrap()).unwrap();
        database.lock().unwrap().execute_batch(sql).unwrap();
        drop(database);
        let error = migration::install(&preview, &state).unwrap_err();
        assert!(error.to_string().contains("already contains data"), "{name}: {error:#}");
        let conn = Connection::open(target).unwrap();
        let count: i64 = conn.query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| row.get(0)).unwrap();
        assert_eq!(count, 1, "{name}: existing records remain intact");
    }
}

#[test]
fn retiring_known_projections_does_not_allow_unrelated_schema_upgrade_data_loss() {
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let original = source(&path);
    // Simulate an incompatible source trigger fired by schema initialization.
    // Only the private staging copy may encounter this loss.
    original
        .lock()
        .unwrap()
        .execute_batch(
            "CREATE TRIGGER incompatible_upgrade BEFORE INSERT ON presence_clock
         BEGIN DELETE FROM \"future records\"; END;",
        )
        .unwrap();
    let state = fixture.path().join("amux");
    let error = migration::prepare_local(&path, &state).unwrap_err();
    assert!(
        error
            .to_string()
            .contains("Schema upgrade removed source rows")
    );
    assert!(error.to_string().contains("future records"));
    assert!(!state.join("server/application.sqlite").exists());
    let count: i64 = original
        .lock()
        .unwrap()
        .query_row("SELECT COUNT(*) FROM \"future records\"", [], |row| {
            row.get(0)
        })
        .unwrap();
    assert_eq!(
        count, 1,
        "source data stays intact despite rejected staging upgrade"
    );
}

#[test]
fn legacy_presence_projections_upgrade_with_canonical_identities_and_original_snapshot_preserved() {
    use rusqlite::params;
    use serde_json::json;
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let original = source(&path);
    let origin = json!({"session_id":"advertised", "instance_id":"instance", "provider":"codex", "os":"linux"});
    let agent = json!({"session_id":"observer", "instance_id":"instance", "provider":"codex", "os":"linux", "project":"unregistered"});
    {
        let conn = original.lock().unwrap();
        conn.execute_batch(
            "DROP TRIGGER presence_project_deleted;
             DROP TABLE task_target_presence;
             CREATE TABLE task_target_presence (
                project_ident TEXT NOT NULL REFERENCES projects(ident),instance_id TEXT NOT NULL,session_id TEXT NOT NULL,
                origin_json TEXT NOT NULL,generation TEXT NOT NULL,seen_at INTEGER NOT NULL,canonical_remote TEXT NOT NULL,
                PRIMARY KEY(project_ident,instance_id,session_id));
             CREATE TABLE presence_agents (session_id TEXT PRIMARY KEY,source TEXT NOT NULL,agent_json TEXT NOT NULL,
                seen_at INTEGER NOT NULL,output_at INTEGER,draft INTEGER NOT NULL DEFAULT 0,state TEXT NOT NULL DEFAULT 'unknown');
             CREATE TABLE notifier_agents (session_id TEXT PRIMARY KEY, project TEXT NOT NULL, agent_json TEXT NOT NULL);
             INSERT INTO notifier_agents VALUES('stale','demo','{}');
             CREATE TABLE reclaim_busy (session_id TEXT PRIMARY KEY);
             INSERT INTO reclaim_busy VALUES('stale');",
        ).unwrap();
        conn.execute("INSERT INTO task_target_presence VALUES('demo','instance','advertised',?1,'generation',100,'')", params![origin.to_string()]).unwrap();
        conn.execute("INSERT INTO presence_agents(session_id,source,agent_json,seen_at,output_at,state) VALUES('observer','client:observer',?1,100,80,'idle')", params![agent.to_string()]).unwrap();
    }
    let state = fixture.path().join("amux");
    let preview = migration::prepare_local(&path, &state).unwrap();
    for retired in ["presence_agents", "notifier_agents", "reclaim_busy"] {
        assert!(!preview.inventory.tables.contains_key(retired));
        assert!(
            preview.inventory.schema_notes.iter().any(
                |note| note.starts_with(retired) && note.contains("original snapshot retained")
            )
        );
    }
    let installed = migration::install(&preview, &state).unwrap();
    let imported = Connection::open(installed.database).unwrap();
    let advertisement: (String, String, bool) = imported.query_row(
        "SELECT origin_json,generation,advertised FROM task_target_presence WHERE session_id='advertised'", [],
        |row| Ok((row.get(0)?,row.get(1)?,row.get(2)?)),
    ).unwrap();
    assert_eq!(
        advertisement,
        (origin.to_string(), "generation".into(), true)
    );
    let observation: (String, String, bool, i64) = imported.query_row(
        "SELECT agent_json,source,advertised,output_at FROM task_target_presence WHERE session_id='observer'", [],
        |row| Ok((row.get(0)?,row.get(1)?,row.get(2)?,row.get(3)?)),
    ).unwrap();
    assert_eq!(
        observation,
        (agent.to_string(), "client:observer".into(), false, 80)
    );
    let backup = state.join("server").join(format!(
        "legacy-source-{}.sqlite",
        preview.inventory.source_sha256
    ));
    let backup = Connection::open(backup).unwrap();
    for retired in ["presence_agents", "notifier_agents", "reclaim_busy"] {
        assert_eq!(
            backup
                .query_row(&format!("SELECT COUNT(*) FROM {retired}"), [], |row| row
                    .get::<_, i64>(0))
                .unwrap(),
            1
        );
        assert_eq!(
            original
                .lock()
                .unwrap()
                .query_row(&format!("SELECT COUNT(*) FROM {retired}"), [], |row| row
                    .get::<_, i64>(0))
                .unwrap(),
            1
        );
    }
}

#[test]
fn receipt_write_failure_can_be_retried_without_losing_the_installed_database() {
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    let _source = source(&path);
    let state = fixture.path().join("amux");
    let preview = migration::prepare_local(&path, &state).unwrap();
    fs::create_dir(state.join("server/migration.json")).unwrap();
    assert!(
        migration::install(&preview, &state)
            .unwrap_err()
            .to_string()
            .contains("Database installed")
    );
    assert!(state.join("server/application.sqlite").is_file());
    fs::remove_dir(state.join("server/migration.json")).unwrap();
    let installed = migration::install(&preview, &state).unwrap();
    assert!(installed.receipt.is_file());
}

#[tokio::test]
async fn remote_snapshot_checks_auth_checksum_and_unsupported_gateways_without_partial_install() {
    use axum::{
        Router,
        body::Body,
        http::{HeaderMap, Response, StatusCode},
        routing::get,
    };
    use sha2::{Digest, Sha256};
    let fixture = fixture();
    let path = fixture.path().join("legacy.sqlite");
    drop(source(&path));
    let bytes = fs::read(&path).unwrap();
    let checksum = format!("{:x}", Sha256::digest(&bytes));
    let router = Router::new().route(
        "/v1/admin/migration/snapshot",
        get(move |headers: HeaderMap| {
            let bytes = bytes.clone();
            let checksum = checksum.clone();
            async move {
                if headers
                    .get("authorization")
                    .and_then(|value| value.to_str().ok())
                    != Some("Bearer fixture-key")
                {
                    return Response::builder()
                        .status(StatusCode::UNAUTHORIZED)
                        .body(Body::empty())
                        .unwrap();
                }
                Response::builder()
                    .header("content-type", "application/vnd.sqlite3")
                    .header("content-length", bytes.len())
                    .header("X-Amux-Snapshot-Sha256", checksum)
                    .body(Body::from(bytes))
                    .unwrap()
            }
        }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        axum::serve(listener, router).await.unwrap();
    });
    let state = fixture.path().join("remote");
    let denied = migration::prepare_remote(&url, "secret-never-logged", &state)
        .await
        .unwrap_err()
        .to_string();
    assert!(denied.contains("401") && !denied.contains("secret-never-logged"));
    let preview = migration::prepare_remote(&url, "fixture-key", &state)
        .await
        .unwrap();
    assert_eq!(preview.inventory.tables["gateway_memories"], 1);
    migration::install(&preview, &state).unwrap();
    server.abort();
    let router = Router::new().route(
        "/v1/admin/migration/snapshot",
        get(|| async {
            Response::builder()
                .header("content-length", 8)
                .header("X-Amux-Snapshot-Sha256", "0".repeat(64))
                .body(Body::from("bad-data"))
                .unwrap()
        }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        axum::serve(listener, router).await.unwrap();
    });
    let state = fixture.path().join("corrupt");
    assert!(
        migration::prepare_remote(&url, "fixture-key", &state)
            .await
            .unwrap_err()
            .to_string()
            .contains("checksum mismatch")
    );
    assert!(!state.join("server/application.sqlite").exists());
    assert_eq!(fs::read_dir(state.join("server")).unwrap().count(), 0);
    server.abort();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        axum::serve(listener, Router::new()).await.unwrap();
    });
    assert!(
        migration::prepare_remote(&url, "fixture-key", &state)
            .await
            .unwrap_err()
            .to_string()
            .contains("does not provide migration exports")
    );
    server.abort();
}
