package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const oldRouterDeliveryStateCheck = `CHECK(state IN ('pending','running','retrying','completed','stopped','skipped'))`

func TestMigrateLegacyRouterDeliveriesPreservesDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.sqlite")
	db := openRouterMigrationDB(t, path)
	_, err := db.Exec(`CREATE TABLE router_events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT, uid TEXT NOT NULL UNIQUE,
		id TEXT NOT NULL, version INTEGER NOT NULL, payload TEXT NOT NULL,
		source TEXT NOT NULL, emitted_at INTEGER NOT NULL
	);
	CREATE TABLE router_deliveries (
		event_seq INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,
		plugin TEXT NOT NULL,
		state TEXT NOT NULL ` + oldRouterDeliveryStateCheck + `,
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at INTEGER NOT NULL,
		last_error TEXT,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY(event_seq,plugin)
	);
	CREATE TABLE delivery_notes (
		event_seq INTEGER NOT NULL, plugin TEXT NOT NULL, note TEXT NOT NULL,
		FOREIGN KEY(event_seq,plugin) REFERENCES router_deliveries(event_seq,plugin) ON DELETE CASCADE
	);
	CREATE INDEX idx_router_deliveries_open ON router_deliveries(state,next_attempt_at);
	CREATE TRIGGER router_delivery_touch AFTER UPDATE OF state ON router_deliveries
	BEGIN UPDATE router_deliveries SET updated_at=updated_at+1 WHERE event_seq=NEW.event_seq AND plugin=NEW.plugin; END;
	CREATE VIEW router_delivery_view AS SELECT event_seq,plugin,state,attempts,last_error FROM router_deliveries;
	INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at) VALUES(42,'original-event-uid','task.updated',1,'{}','tasks',10);
	INSERT INTO router_deliveries(rowid,event_seq,plugin,state,attempts,next_attempt_at,last_error,updated_at)
	VALUES(314,42,'fixture','retrying',2,25,'offline',30);
	INSERT INTO delivery_notes VALUES(42,'fixture','retained child');`)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrateLegacyRouterDeliveries(context.Background(), db, path); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, db, "SELECT uid FROM router_events WHERE seq=42", "original-event-uid")
	assertEqual(t, db, "SELECT rowid FROM router_deliveries WHERE event_seq=42 AND plugin='fixture'", int64(314))
	assertEqual(t, db, "SELECT state FROM router_deliveries WHERE event_seq=42 AND plugin='fixture'", "retrying")
	assertEqual(t, db, "SELECT attempts FROM router_deliveries WHERE event_seq=42 AND plugin='fixture'", int64(2))
	assertEqual(t, db, "SELECT last_error FROM router_deliveries WHERE event_seq=42 AND plugin='fixture'", "offline")
	assertEqual(t, db, "SELECT note FROM delivery_notes", "retained child")
	assertEqual(t, db, "SELECT state FROM router_delivery_view WHERE event_seq=42", "retrying")
	assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_router_deliveries_open'", int64(1))
	assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='router_delivery_touch'", int64(1))
	assertSchemaContainsFragment(t, db, "router_deliveries", currentRouterDeliveryStateCheck)
	if _, err := db.Exec("UPDATE router_deliveries SET state='failed' WHERE event_seq=42 AND plugin='fixture'"); err != nil {
		t.Fatalf("failed state rejected after upgrade: %v", err)
	}
	assertEqual(t, db, "SELECT updated_at FROM router_deliveries WHERE event_seq=42", int64(31))
	assertEqual(t, db, "SELECT count(*) FROM pragma_foreign_key_check", int64(0))
	assertEqual(t, db, "PRAGMA foreign_keys", int64(1))
	assertEqual(t, db, "PRAGMA legacy_alter_table", int64(0))

	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*", "database.sqlite"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups %v: %v", backups, err)
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	assertEqual(t, backup, "SELECT state FROM router_deliveries WHERE event_seq=42", "retrying")
	assertSchemaContainsFragment(t, backup, "router_deliveries", oldRouterDeliveryStateCheck)

	if _, err := db.Exec("DELETE FROM router_events WHERE seq=42"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, db, "SELECT count(*) FROM router_deliveries", int64(0))
	assertEqual(t, db, "SELECT count(*) FROM delivery_notes", int64(0))
}

func TestMigrateLegacyRouterDeliveriesReopenAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.sqlite")
	db := openRouterMigrationDB(t, path)
	createLegacyRouterDelivery(t, db, oldRouterDeliveryStateCheck)
	if err := migrateLegacyRouterDeliveries(context.Background(), db, path); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openRouterMigrationDB(t, path)
	defer reopened.Close()
	if err := migrateLegacyRouterDeliveries(context.Background(), reopened, path); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if _, err := reopened.Exec("INSERT INTO router_events(uid,id,version,payload,source,emitted_at) VALUES('again','x',1,'{}','test',1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Exec("INSERT INTO router_deliveries(event_seq,plugin,state,next_attempt_at,updated_at) VALUES((SELECT seq FROM router_events WHERE uid='again'),'p','failed',0,0)"); err != nil {
		t.Fatalf("failed state rejected after reopen: %v", err)
	}
	assertEqual(t, reopened, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='router_deliveries_state_rebuild'", int64(0))
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*"))
	if len(backups) != 1 {
		t.Fatalf("idempotent migration created backups: %v", backups)
	}
}

func TestMigrateLegacyRouterDeliveriesSkipAndReject(t *testing.T) {
	t.Run("missing table", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.sqlite")
		db := openRouterMigrationDB(t, path)
		defer db.Close()
		if err := migrateLegacyRouterDeliveries(context.Background(), db, path); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown state list", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unknown.sqlite")
		db := openRouterMigrationDB(t, path)
		defer db.Close()
		createLegacyRouterDelivery(t, db, `CHECK(state IN ('pending','running','retrying','completed','stopped','skipped','FAILED'))`)
		if err := migrateLegacyRouterDeliveries(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "unrecognized router_deliveries state constraint") {
			t.Fatalf("expected fail-closed state error, got %v", err)
		}
		assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='router_deliveries_state_rebuild'", int64(0))
	})
	t.Run("keyword case and spacing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "formatted.sqlite")
		db := openRouterMigrationDB(t, path)
		defer db.Close()
		createLegacyRouterDelivery(t, db, `cHeCk ( STATE in ( 'pending' , 'running' , 'retrying' , 'completed' , 'stopped' , 'skipped' ) )`)
		if err := migrateLegacyRouterDeliveries(context.Background(), db, path); err != nil {
			t.Fatal(err)
		}
		assertSchemaContainsFragment(t, db, "router_deliveries", currentRouterDeliveryStateCheck)
	})
}

func openRouterMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createLegacyRouterDelivery(t *testing.T, db *sql.DB, check string) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE router_events(seq INTEGER PRIMARY KEY AUTOINCREMENT,uid TEXT NOT NULL UNIQUE,id TEXT NOT NULL,version INTEGER NOT NULL,payload TEXT NOT NULL,source TEXT NOT NULL,emitted_at INTEGER NOT NULL);
	CREATE TABLE router_deliveries(event_seq INTEGER NOT NULL REFERENCES router_events(seq) ON DELETE CASCADE,plugin TEXT NOT NULL,state TEXT NOT NULL ` + check + `,attempts INTEGER NOT NULL DEFAULT 0,next_attempt_at INTEGER NOT NULL,last_error TEXT,updated_at INTEGER NOT NULL,PRIMARY KEY(event_seq,plugin));`)
	if err != nil {
		t.Fatal(err)
	}
}

func assertSchemaContainsFragment(t *testing.T, db *sql.DB, table, fragment string) {
	t.Helper()
	var ddl string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, fragment) {
		t.Fatalf("%s schema does not contain %q: %s", table, fragment, ddl)
	}
}
