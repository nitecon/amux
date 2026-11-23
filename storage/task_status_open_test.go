package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenUpgradesLegacyTaskStatuses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE projects(ident TEXT PRIMARY KEY,channel_name TEXT NOT NULL,room_id TEXT NOT NULL,last_msg_id TEXT,created_at INTEGER NOT NULL);
 INSERT INTO projects VALUES('legacy','legacy','',NULL,1);
 CREATE TABLE tasks(id TEXT PRIMARY KEY,project_ident TEXT NOT NULL REFERENCES projects(ident),title TEXT NOT NULL,description TEXT,details TEXT,status TEXT NOT NULL DEFAULT 'todo' CHECK(status IN ('todo','in_progress','done')),rank INTEGER NOT NULL DEFAULT 0,labels TEXT,hostname TEXT,owner_agent_id TEXT,reporter TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,started_at INTEGER,done_at INTEGER);
 INSERT INTO tasks(id,project_ident,title,status,reporter,created_at,updated_at) VALUES('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','legacy','Preserve me','todo','fixture',1,1);
 PRAGMA user_version=77;`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var title string
	if err := s.db.QueryRow("SELECT title FROM tasks").Scan(&title); err != nil || title != "Preserve me" {
		t.Fatalf("title %q: %v", title, err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET status='cancelled'"); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 77 {
		t.Fatalf("version %d: %v", version, err)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*", "database.sqlite"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups %v: %v", backups, err)
	}
}

func TestOpenRejectsFutureVersionBeforeLegacyRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE amux_go_migrations(version INTEGER PRIMARY KEY); INSERT INTO amux_go_migrations VALUES(999); CREATE TABLE tasks(id TEXT PRIMARY KEY,status TEXT CHECK(status IN ('todo','in_progress','done')));`)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := Open(context.Background(), path); err == nil {
		s.Close()
		t.Fatal("accepted future schema")
	}
	var ddl string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name='tasks'").Scan(&ddl); err != nil || strings.Contains(ddl, "cancelled") {
		t.Fatalf("mutated future schema %s: %v", ddl, err)
	}
	backups, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*"))
	if len(backups) != 0 {
		t.Fatalf("unexpected backups %v", backups)
	}
}
