package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPresenceAuthorityRebuildPreservesOwnedSchemaAndChildren(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "presence.sqlite")
	db := openRouterMigrationDB(t, path)
	if _, err := db.Exec(`CREATE TABLE projects(ident TEXT PRIMARY KEY);
		INSERT INTO projects VALUES('known');
		CREATE TABLE amux_presence_agents(
		 project_ident TEXT NOT NULL REFERENCES projects(ident) ON DELETE CASCADE,
		 instance_id TEXT NOT NULL,session_id TEXT NOT NULL,source TEXT NOT NULL,
		 state TEXT NOT NULL CHECK(state IN('unknown','busy','idle','gone')),
		 custom TEXT,PRIMARY KEY(project_ident,instance_id,session_id));
		CREATE TABLE presence_notes(project_ident TEXT,instance_id TEXT,session_id TEXT,note TEXT,
		 FOREIGN KEY(project_ident,instance_id,session_id) REFERENCES amux_presence_agents(project_ident,instance_id,session_id) ON DELETE CASCADE);
		CREATE INDEX retained_presence_source ON amux_presence_agents(source);
		CREATE TABLE presence_audit(value TEXT);
		CREATE TRIGGER retained_presence_update AFTER UPDATE ON amux_presence_agents BEGIN INSERT INTO presence_audit VALUES(NEW.custom); END;
		CREATE VIEW retained_presence_view AS SELECT source,custom FROM amux_presence_agents;
		INSERT INTO amux_presence_agents(rowid,project_ident,instance_id,session_id,source,state,custom) VALUES(42,'known','instance','session','source','busy','retained');
		INSERT INTO presence_notes VALUES('known','instance','session','child');`); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyPresenceAuthority(ctx, db, path); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, db, "SELECT rowid FROM amux_presence_agents", int64(42))
	assertEqual(t, db, "SELECT custom FROM retained_presence_view", "retained")
	assertEqual(t, db, "SELECT note FROM presence_notes", "child")
	assertEqual(t, db, "SELECT COUNT(*) FROM pragma_foreign_key_list('amux_presence_agents')", int64(0))
	assertEqual(t, db, "SELECT COUNT(*) FROM pragma_foreign_key_check", int64(0))
	assertEqual(t, db, "PRAGMA foreign_keys", int64(1))
	if _, err := db.Exec("UPDATE amux_presence_agents SET custom='updated'"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, db, "SELECT value FROM presence_audit", "updated")
	if _, err := db.Exec("INSERT INTO amux_presence_agents VALUES('unresolved','new','new','source','unknown','observed')"); err != nil {
		t.Fatalf("unresolved observation rejected after rebuild: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*", "database.sqlite"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("protected backups: %v %v", backups, err)
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	assertEqual(t, backup, "SELECT custom FROM amux_presence_agents", "retained")
	assertEqual(t, backup, "SELECT COUNT(*) FROM pragma_foreign_key_list('amux_presence_agents')", int64(1))
	if err := migrateLegacyPresenceAuthority(ctx, db, path); err != nil {
		t.Fatalf("idempotent rebuild: %v", err)
	}
	backups, err = filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*", "database.sqlite"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("reopen repeated backup: %v %v", backups, err)
	}
	if _, err := db.Exec("DELETE FROM amux_presence_agents WHERE project_ident='known'"); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, db, "SELECT COUNT(*) FROM presence_notes", int64(0))
}

func TestPresenceAuthorityRefusesUnrecognizedReferenceBeforeBackup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "unknown.sqlite")
	db := openRouterMigrationDB(t, path)
	if _, err := db.Exec(`CREATE TABLE projects(ident TEXT PRIMARY KEY);
		CREATE TABLE amux_presence_agents(project_ident TEXT NOT NULL REFERENCES projects(ident) ON DELETE RESTRICT,instance_id TEXT,session_id TEXT);`); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyPresenceAuthority(ctx, db, path); err == nil {
		t.Fatal("unrecognized reference accepted")
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*", "database.sqlite"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("refusal created backup: %v %v", backups, err)
	}
	assertEqual(t, db, "SELECT COUNT(*) FROM pragma_foreign_key_list('amux_presence_agents')", int64(1))
}
