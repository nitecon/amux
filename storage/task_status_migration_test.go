package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateLegacyTaskStatusPreservesDatabase(t *testing.T) {
	for _, fixture := range []struct{ name, check string }{
		{"old three", `CHECK(status IN ('todo','in_progress','done'))`},
		{"old four", `CHECK(status IN ('todo','in_progress','done','cancelled'))`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path, db := legacyStatusFixture(t, fixture.check)
			if err := migrateLegacyTaskStatus(context.Background(), db, path); err != nil {
				t.Fatal(err)
			}
			assertEqual(t, db, "SELECT title FROM tasks WHERE id='t1'", "kept title")
			assertEqual(t, db, "SELECT content FROM task_comments WHERE id='c1'", "kept comment")
			assertEqual(t, db, "SELECT detail FROM task_history WHERE task_id='t1'", "kept history")
			assertEqual(t, db, "SELECT marker FROM external_rows WHERE id=1", "kept external row")
			assertEqual(t, db, "SELECT task_title FROM task_title_view WHERE id='t1'", "kept title")
			if _, err := db.Exec("UPDATE external_rows SET marker='triggered' WHERE id=1"); err != nil {
				t.Fatalf("external trigger failed: %v", err)
			}
			assertEqual(t, db, "SELECT title FROM tasks WHERE id='t1'", "triggered")
			assertEqual(t, db, "SELECT rowid FROM tasks WHERE id='t1'", int64(55))
			if _, err := db.Exec("UPDATE tasks SET status='user_review' WHERE id='t1'"); err != nil {
				t.Fatalf("widened status rejected: %v", err)
			}
			assertEqual(t, db, "SELECT count(*) FROM task_audit WHERE task_id='t1'", int64(1))
			assertEqual(t, db, "SELECT count(*) FROM task_comments WHERE task_id='t1'", int64(1))
			assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='index' AND name='tasks_title_idx'", int64(1))
			assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='tasks_audit_update'", int64(1))
			assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='external_task_update'", int64(1))
			assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='view' AND name='task_title_view'", int64(1))
			assertEqual(t, db, "SELECT count(*) FROM pragma_foreign_key_check", int64(0))
			assertEqual(t, db, "PRAGMA foreign_keys", int64(1))
			assertEqual(t, db, "PRAGMA legacy_alter_table", int64(0))
			assertEqual(t, db, "PRAGMA user_version", int64(91))

			backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*", "database.sqlite"))
			if err != nil || len(backups) != 1 {
				t.Fatalf("backups %v: %v", backups, err)
			}
			info, err := os.Stat(backups[0])
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("backup mode %o, want 600", info.Mode().Perm())
			}
			backupDirInfo, err := os.Stat(filepath.Dir(backups[0]))
			if err != nil || (runtime.GOOS != "windows" && backupDirInfo.Mode().Perm() != 0700) {
				t.Fatalf("backup directory mode %v: %v", backupDirInfo, err)
			}
			backup, err := sql.Open("sqlite", backups[0])
			if err != nil {
				t.Fatal(err)
			}
			defer backup.Close()
			assertEqual(t, backup, "SELECT title FROM tasks WHERE id='t1'", "kept title")
			assertSchemaContains(t, backup, fixture.check)
			assertEqual(t, backup, "PRAGMA user_version", int64(91))
		})
	}
}

func TestMigrateLegacyTaskStatusSkipAndReject(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		path, db := openStatusDB(t)
		if err := migrateLegacyTaskStatus(context.Background(), db, path); err != nil {
			t.Fatal(err)
		}
		assertNoStatusBackup(t, path)
	})
	t.Run("current schema", func(t *testing.T) {
		path, db := openStatusDB(t)
		_, err := db.Exec("CREATE TABLE tasks(id TEXT,status TEXT " + currentTaskStatusCheck + ")")
		if err != nil {
			t.Fatal(err)
		}
		if err := migrateLegacyTaskStatus(context.Background(), db, path); err != nil {
			t.Fatal(err)
		}
		assertNoStatusBackup(t, path)
	})
	t.Run("unknown uppercase literal", func(t *testing.T) {
		path, db := openStatusDB(t)
		_, err := db.Exec(`CREATE TABLE tasks(id TEXT,status TEXT CHECK(status IN ('TODO','in_progress','done')))`)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrateLegacyTaskStatus(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "unrecognized tasks status constraint") {
			t.Fatalf("expected fail-closed status constraint error, got %v", err)
		}
		assertNoStatusBackup(t, path)
	})
}

func TestMigrateLegacyTaskStatusRollsBack(t *testing.T) {
	t.Run("temporary name collision", func(t *testing.T) {
		path, db := legacyStatusFixture(t, `CHECK(status IN ('todo','in_progress','done'))`)
		if _, err := db.Exec("CREATE TABLE tasks_status_rebuild(sentinel TEXT); INSERT INTO tasks_status_rebuild VALUES('untouched')"); err != nil {
			t.Fatal(err)
		}
		if err := migrateLegacyTaskStatus(context.Background(), db, path); err == nil {
			t.Fatal("expected collision failure")
		}
		assertEqual(t, db, "SELECT title FROM tasks WHERE id='t1'", "kept title")
		assertEqual(t, db, "SELECT sentinel FROM tasks_status_rebuild", "untouched")
		assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='tasks_audit_update'", int64(1))
		assertEqual(t, db, "PRAGMA foreign_keys", int64(1))
	})
	t.Run("preexisting foreign key violation", func(t *testing.T) {
		path, db := legacyStatusFixture(t, `CHECK(status IN ('todo','in_progress','done'))`)
		if _, err := db.Exec("PRAGMA foreign_keys=OFF; INSERT INTO task_comments(id,task_id,content) VALUES('orphan','missing','orphan'); PRAGMA foreign_keys=ON"); err != nil {
			t.Fatal(err)
		}
		if err := migrateLegacyTaskStatus(context.Background(), db, path); err == nil || !strings.Contains(err.Error(), "foreign key violation") {
			t.Fatalf("expected foreign key check failure, got %v", err)
		}
		assertEqual(t, db, "SELECT title FROM tasks WHERE id='t1'", "kept title")
		assertSchemaContains(t, db, `CHECK(status IN ('todo','in_progress','done'))`)
		assertEqual(t, db, "SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='tasks_audit_update'", int64(1))
		assertEqual(t, db, "PRAGMA foreign_keys", int64(1))
	})
}

func legacyStatusFixture(t *testing.T, check string) (string, *sql.DB) {
	t.Helper()
	path, db := openStatusDB(t)
	_, err := db.Exec(`CREATE TABLE projects(id TEXT PRIMARY KEY);
CREATE TABLE tasks(
 id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL REFERENCES projects(id),
 title TEXT NOT NULL, status TEXT NOT NULL ` + check + `, /* retained table comment */
 extra TEXT DEFAULT 'unchanged'
);
CREATE TABLE task_comments(id TEXT PRIMARY KEY, task_id TEXT REFERENCES tasks(id) ON DELETE CASCADE, content TEXT);
CREATE TABLE task_history(id INTEGER PRIMARY KEY, task_id TEXT, detail TEXT);
CREATE TABLE external_rows(id INTEGER PRIMARY KEY, task_id TEXT REFERENCES tasks(id), marker TEXT);
CREATE TABLE task_audit(task_id TEXT, old_status TEXT, new_status TEXT);
CREATE INDEX tasks_title_idx ON tasks(title);
CREATE TRIGGER tasks_audit_update AFTER UPDATE OF status ON tasks BEGIN
 INSERT INTO task_audit VALUES(OLD.id,OLD.status,NEW.status);
END;
CREATE TRIGGER external_task_update AFTER UPDATE OF marker ON external_rows BEGIN
 UPDATE tasks SET title=NEW.marker WHERE id=NEW.task_id;
END;
CREATE VIEW task_title_view AS SELECT id,title AS task_title FROM tasks;
INSERT INTO projects VALUES('p1');
INSERT INTO tasks(rowid,id,project_id,title,status,extra) VALUES(55,'t1','p1','kept title','todo','unchanged');
INSERT INTO task_comments VALUES('c1','t1','kept comment');
INSERT INTO task_history VALUES(1,'t1','kept history');
INSERT INTO external_rows VALUES(1,'t1','kept external row');
PRAGMA user_version=91;`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return path, db
}

func openStatusDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
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
	return path, db
}

func assertNoStatusBackup(t *testing.T, path string) {
	t.Helper()
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".task-status-backup-*"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("unexpected backups %v: %v", backups, err)
	}
}

func assertEqual(t *testing.T, db *sql.DB, query string, want any) {
	t.Helper()
	var got any
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got != want {
		t.Fatalf("%s = %#v, want %#v", query, got, want)
	}
}

func assertSchemaContains(t *testing.T, db *sql.DB, fragment string) {
	t.Helper()
	var ddl string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'").Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, fragment) || !strings.Contains(ddl, "retained table comment") {
		t.Fatalf("tasks schema did not retain expected SQL/comment: %s", ddl)
	}
}
