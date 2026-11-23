package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLegacyTaskRowsMigrateWithoutRequesterOrRewritingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-requester.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `PRAGMA foreign_keys=ON;
CREATE TABLE projects(ident TEXT PRIMARY KEY,channel_name TEXT NOT NULL,room_id TEXT NOT NULL,last_msg_id TEXT,created_at INTEGER NOT NULL);
CREATE TABLE tasks(
 id TEXT PRIMARY KEY,project_ident TEXT NOT NULL REFERENCES projects(ident),title TEXT NOT NULL,description TEXT,details TEXT,
 status TEXT NOT NULL DEFAULT 'todo' CHECK(status IN ('todo','in_progress','user_review','done','cancelled')),rank INTEGER NOT NULL DEFAULT 0,
 labels TEXT,hostname TEXT,owner_agent_id TEXT,reporter TEXT NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,started_at INTEGER,done_at INTEGER
);
CREATE TABLE task_comments(id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,author TEXT NOT NULL,
 author_type TEXT NOT NULL CHECK(author_type IN ('agent','user','system')),content TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE TABLE external_rows(id INTEGER PRIMARY KEY,task_id TEXT NOT NULL REFERENCES tasks(id),marker TEXT NOT NULL);
CREATE TABLE task_audit(task_id TEXT NOT NULL);
CREATE TRIGGER audit_task_updates AFTER UPDATE ON tasks BEGIN INSERT INTO task_audit VALUES(NEW.id); END;
INSERT INTO projects VALUES('tasks','legacy-channel','legacy-room','17',101);
INSERT INTO tasks(rowid,id,project_ident,title,description,details,status,rank,labels,hostname,owner_agent_id,reporter,created_at,updated_at,started_at,done_at)
 VALUES(47,'legacy-task','tasks','Preserved title','description','details','user_review',9,'["legacy"]','host-a','agent-a','reporter-a',111,222,123,0);
INSERT INTO task_comments VALUES('legacy-comment','legacy-task','author-a','agent','kept comment',333);
INSERT INTO external_rows VALUES(8,'legacy-task','unrelated marker');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	const originalTask = `SELECT rowid,id,project_ident,title,description,details,status,rank,labels,hostname,owner_agent_id,reporter,created_at,updated_at,started_at,done_at FROM tasks WHERE id='legacy-task'`
	var before []any
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := legacy.QueryContext(ctx, originalTask)
	if err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	cols, err := rows.Columns()
	if err != nil {
		rows.Close()
		legacy.Close()
		t.Fatal(err)
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if !rows.Next() {
		rows.Close()
		legacy.Close()
		t.Fatal("legacy task fixture missing")
	}
	if err := rows.Scan(ptrs...); err != nil {
		rows.Close()
		legacy.Close()
		t.Fatal(err)
	}
	before = append(before, values...)
	if rows.Next() {
		rows.Close()
		legacy.Close()
		t.Fatal("duplicate legacy task fixture")
	}
	if err := rows.Close(); err != nil {
		legacy.Close()
		t.Fatal(err)
	}
	legacy.Close()

	for pass := 0; pass < 2; pass++ {
		store, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open pass %d: %v", pass+1, err)
		}
		task, err := store.Task(ctx, "tasks", "legacy-task")
		if err != nil || len(task.Task.Requester) != 0 {
			t.Fatalf("legacy task requester=%+v err=%v", task.Task.Requester, err)
		}
		var after []any
		current, err := store.db.QueryContext(ctx, originalTask)
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		currentValues := make([]any, len(cols))
		currentPtrs := make([]any, len(cols))
		for i := range currentValues {
			currentPtrs[i] = &currentValues[i]
		}
		if !current.Next() {
			current.Close()
			store.Close()
			t.Fatal("task disappeared during migration")
		}
		if err := current.Scan(currentPtrs...); err != nil {
			current.Close()
			store.Close()
			t.Fatal(err)
		}
		after = append(after, currentValues...)
		if err := current.Close(); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			store.Close()
			t.Fatalf("legacy task row rewritten: before=%#v after=%#v", before, after)
		}
		var auditCount, commentCount int
		var marker string
		var schema string
		if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM task_audit").Scan(&auditCount); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM task_comments WHERE id='legacy-comment'").Scan(&commentCount); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := store.db.QueryRowContext(ctx, "SELECT marker FROM external_rows WHERE id=8").Scan(&marker); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if err := store.db.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'").Scan(&schema); err != nil {
			store.Close()
			t.Fatal(err)
		}
		if auditCount != 0 || commentCount != 1 || marker != "unrelated marker" || !strings.Contains(schema, "'user_review'") || !strings.Contains(schema, "'cancelled'") {
			store.Close()
			t.Fatalf("migration changed legacy artifacts: audit=%d comments=%d marker=%q schema=%s", auditCount, commentCount, marker, schema)
		}
		var requester sql.NullString
		if err := store.db.QueryRowContext(ctx, "SELECT requester_json FROM tasks WHERE id='legacy-task'").Scan(&requester); err != nil || requester.Valid {
			store.Close()
			t.Fatalf("legacy requester column should remain NULL: %+v err=%v", requester, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}

	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var fkViolations int
	if err := check.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&fkViolations); err != nil || fkViolations != 0 {
		t.Fatalf("foreign key check=%d err=%v", fkViolations, err)
	}
}
