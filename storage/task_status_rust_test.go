package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenMigratesFullRustTaskStatusProfiles(t *testing.T) {
	ctx := context.Background()
	schema, err := os.ReadFile("testdata/task_status_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	current := `CHECK(status IN ('todo','in_progress','user_review','done','cancelled'))`
	if count := strings.Count(string(schema), current); count != 1 {
		t.Fatalf("Rust schema has %d task status checks, want exactly one", count)
	}

	for _, fixture := range []struct {
		name  string
		check string
	}{
		{"old-three", `CHECK(status IN ('todo','in_progress','done'))`},
		{"old-four", `CHECK(status IN ('todo','in_progress','done','cancelled'))`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rust-profile.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
				t.Fatal(err)
			}
			legacy := strings.Replace(string(schema), current, fixture.check, 1)
			if _, err := db.ExecContext(ctx, legacy); err != nil {
				t.Fatalf("load full Rust schema: %v", err)
			}
			seedRustTaskProfile(t, ctx, db)
			originalSchema := snapshotRustSchema(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			store, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("Open full Rust profile: %v", err)
			}
			defer store.Close()
			assertRustSchemaPreserved(t, store.db, originalSchema)

			assertEqual(t, store.db, "SELECT title FROM tasks WHERE id='rust-source'", "Rust source task")
			assertEqual(t, store.db, "SELECT content FROM task_comments WHERE id='rust-comment'", "Rust comment")
			assertEqual(t, store.db, "SELECT COUNT(*) FROM task_reviews WHERE source_task_id='rust-source' AND review_task_id='rust-review' AND stage='code'", int64(1))
			assertEqual(t, store.db, "SELECT output FROM execution_runs WHERE id='rust-run'", "retained execution history")
			assertEqual(t, store.db, "SELECT COUNT(*) FROM task_events WHERE json_extract(task_json,'$.id')='rust-source'", int64(2))
			assertEqual(t, store.db, "SELECT COUNT(*) FROM router_task_outbox WHERE json_extract(task_json,'$.id')='rust-source'", int64(2))
			assertEqual(t, store.db, "SELECT message FROM task_event_deliveries WHERE consumer_id='rust-consumer'", "retained stream receipt")
			assertEqual(t, store.db, "SELECT body FROM execution_attempts WHERE run_id='rust-run' AND position=0", `{"attempt":1}`)
			assertEqual(t, store.db, "SELECT payload FROM router_events WHERE uid='rust-router-event'", `{"task":"rust-source"}`)
			assertEqual(t, store.db, "SELECT state FROM router_deliveries WHERE event_seq=(SELECT seq FROM router_events WHERE uid='rust-router-event') AND plugin='fixture'", "completed")

			for _, trigger := range []string{
				"task_event_created", "task_event_commented", "task_event_cancelled",
				"router_task_created", "router_task_updated", "router_task_commented", "router_task_cancelled",
			} {
				assertEqual(t, store.db, "SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='"+trigger+"'", int64(1))
			}
			assertEqual(t, store.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
			assertEqual(t, store.db, "PRAGMA foreign_keys", int64(1))
			var migratedDDL string
			if err := store.db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'").Scan(&migratedDDL); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(migratedDDL, current) {
				t.Fatalf("task status constraint not widened: %s", migratedDDL)
			}

			actor := taskActor()
			if _, err := store.CancelTask(ctx, "rust-project", "rust-source", "legacy migration check", json.RawMessage(`null`), actor); err != nil {
				t.Fatalf("cancel migrated Rust task: %v", err)
			}
			assertEqual(t, store.db, "SELECT COUNT(*) FROM task_events WHERE kind='task_cancelled' AND json_extract(task_json,'$.id')='rust-source'", int64(1))
			assertEqual(t, store.db, "SELECT COUNT(*) FROM router_task_outbox WHERE kind='cancelled' AND json_extract(task_json,'$.id')='rust-source' AND actor_json IS NOT NULL", int64(1))
			assertEqual(t, store.db, "SELECT json_extract(actor_json,'$.session_id') FROM router_task_outbox WHERE kind='cancelled' AND json_extract(task_json,'$.id')='rust-source'", actor.SessionID)
			assertEqual(t, store.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
			if _, err := store.db.ExecContext(ctx, `INSERT INTO task_dispatches(task_id,project_ident,document_json)
				VALUES('rust-source','rust-project','{}')`); err != nil {
				t.Fatal(err)
			}
			if deleted, err := store.DeleteProject(ctx, "rust-project", "rust-project"); err != nil || !deleted {
				t.Fatalf("delete migrated Rust project: deleted=%v err=%v", deleted, err)
			}
			assertEqual(t, store.db, "SELECT COUNT(*) FROM task_dispatches", int64(0))
			assertEqual(t, store.db, "SELECT COUNT(*) FROM execution_attempts", int64(0))
			assertEqual(t, store.db, "SELECT COUNT(*) FROM router_events WHERE uid='rust-router-event'", int64(0))
			assertEqual(t, store.db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
		})
	}
}

func seedRustTaskProfile(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	statements := []string{
		`INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('rust-project','fixture','',1)`,
		`INSERT OR IGNORE INTO task_mutation_context(id,origin_json) VALUES(1,NULL)`,
		`INSERT INTO tasks(id,project_ident,title,status,labels,reporter,created_at,updated_at) VALUES('rust-source','rust-project','Rust source task','todo','[]','fixture',1,1)`,
		`INSERT INTO tasks(id,project_ident,title,status,labels,reporter,created_at,updated_at) VALUES('rust-review','rust-project','Rust review task','todo','["task-review","review-of:rust-source"]','fixture',2,2)`,
		`INSERT INTO task_comments(id,task_id,author,author_type,content,created_at) VALUES('rust-comment','rust-source','fixture','user','Rust comment',3)`,
		`INSERT INTO task_reviews(source_task_id,source_done_at,review_task_id,stage) VALUES('rust-source',4,'rust-review','code')`,
		`INSERT INTO execution_runs(id,project_ident,task_id,trigger,dedup_key,status,created_at,output) VALUES('rust-run','rust-project','rust-source','manual','rust-run-key','completed',5,'retained execution history')`,
		`INSERT INTO execution_attempts(run_id,position,body) VALUES('rust-run',0,'{"attempt":1}')`,
		`INSERT INTO task_stream_consumers(consumer_id,cursor,connection_id,connected,last_seen_at) VALUES('rust-consumer',0,'fixture-connection',1,6)`,
		`INSERT INTO task_event_deliveries(consumer_id,event_id,status,workspace_id,surface_id,message,summary,updated_at) VALUES('rust-consumer',(SELECT id FROM task_events WHERE kind='task_created' AND json_extract(task_json,'$.id')='rust-source' ORDER BY id LIMIT 1),'delivered','workspace','surface','retained stream receipt','receipt',7)`,
		`INSERT INTO router_events(uid,id,version,payload,source,emitted_at) VALUES('rust-router-event','task.created',1,'{"task":"rust-source"}','tasks',8)`,
		`INSERT INTO router_deliveries(event_seq,plugin,state,attempts,next_attempt_at,updated_at) VALUES((SELECT seq FROM router_events WHERE uid='rust-router-event'),'fixture','completed',1,8,8)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed Rust profile (%s): %v", statement, err)
		}
	}
}

type rustSchemaObject struct {
	typeName string
	sql      sql.NullString
}

func snapshotRustSchema(t *testing.T, db *sql.DB) map[string]rustSchemaObject {
	t.Helper()
	rows, err := db.Query("SELECT type,name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	objects := make(map[string]rustSchemaObject)
	for rows.Next() {
		var name string
		var object rustSchemaObject
		if err := rows.Scan(&object.typeName, &name, &object.sql); err != nil {
			t.Fatal(err)
		}
		objects[name] = object
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

func assertRustSchemaPreserved(t *testing.T, db *sql.DB, original map[string]rustSchemaObject) {
	t.Helper()
	current := snapshotRustSchema(t, db)
	for name, before := range original {
		after, ok := current[name]
		if !ok {
			t.Errorf("Rust schema object %q was dropped", name)
			continue
		}
		if before.typeName != after.typeName {
			t.Errorf("Rust schema object %q changed type from %s to %s", name, before.typeName, after.typeName)
		}
		if name != "tasks" && before.sql != after.sql {
			t.Errorf("Rust schema object %q SQL changed unexpectedly:\nbefore: %s\nafter:  %s", name, before.sql.String, after.sql.String)
		}
	}
}
