package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenRetiresLegacyExecutionRuns(t *testing.T) {
	for _, current := range []bool{false, true} {
		name := "legacy"
		if current {
			name = "current"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "amux.db")
			initial, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			schema := `CREATE TABLE execution_runs (
				id TEXT PRIMARY KEY, project_ident TEXT NOT NULL, task_id TEXT, trigger TEXT NOT NULL,
				dedup_key TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL,
				finished_at INTEGER, output TEXT NOT NULL DEFAULT ''
			);`
			if current {
				schema = `CREATE TABLE execution_runs (
					id TEXT PRIMARY KEY, project_ident TEXT NOT NULL, task_id TEXT, trigger TEXT NOT NULL,
					dedup_key TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL,
					finished_at INTEGER, output TEXT NOT NULL DEFAULT '', started_at INTEGER, updated_at INTEGER,
					client TEXT, model TEXT, executor TEXT NOT NULL DEFAULT 'headless', progress TEXT NOT NULL DEFAULT '',
					summary TEXT NOT NULL DEFAULT '', summary_source TEXT NOT NULL DEFAULT 'missing', session_key TEXT,
					last_sequence INTEGER NOT NULL DEFAULT 0
				);`
			}
			_, err = db.ExecContext(ctx, schema+`
				CREATE TABLE execution_attempts(run_id TEXT NOT NULL, position INTEGER NOT NULL, body TEXT NOT NULL);
				INSERT INTO execution_runs(id,project_ident,trigger,dedup_key,status,created_at,output)
				VALUES ('unfinished','history','manual','unfinished-key','running',10,'partial output');
				INSERT INTO execution_runs(id,project_ident,trigger,dedup_key,status,created_at,finished_at,output)
				VALUES ('finished','history','manual','finished-key','completed',20,30,'finished output');
				INSERT INTO execution_attempts VALUES('unfinished',1,'{"kept":true}');`)
			if err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			store, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			var status, progress, output string
			var finishedAt, updatedAt int64
			if err := store.db.QueryRowContext(ctx, `SELECT status,finished_at,updated_at,progress,output
				FROM execution_runs WHERE id='unfinished'`).Scan(&status, &finishedAt, &updatedAt, &progress, &output); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UnixMilli()
			if status != "retired" || finishedAt <= 0 || finishedAt != updatedAt || finishedAt < now-60_000 || finishedAt > now+60_000 || progress != "Legacy executor removed; no work resumed" || output != "partial output" {
				t.Fatalf("retired run status=%q finished=%d updated=%d progress=%q output=%q", status, finishedAt, updatedAt, progress, output)
			}
			var keptStatus, keptOutput string
			if err := store.db.QueryRowContext(ctx, `SELECT status,output FROM execution_runs WHERE id='finished'`).Scan(&keptStatus, &keptOutput); err != nil {
				t.Fatal(err)
			}
			if keptStatus != "completed" || keptOutput != "finished output" {
				t.Fatalf("finished run changed: status=%q output=%q", keptStatus, keptOutput)
			}
			var attempts int
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_attempts WHERE run_id='unfinished' AND body='{"kept":true}'`).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatalf("attempt changed: count=%d err=%v", attempts, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var reopenedFinished, reopenedUpdated int64
			if err := store.db.QueryRowContext(ctx, `SELECT finished_at,updated_at FROM execution_runs WHERE id='unfinished'`).Scan(&reopenedFinished, &reopenedUpdated); err != nil || reopenedFinished != finishedAt || reopenedUpdated != updatedAt {
				t.Fatalf("second open changed retirement timestamps: finished=%d updated=%d err=%v", reopenedFinished, reopenedUpdated, err)
			}
		})
	}
}

func TestOpenWithoutExecutionHistoryDoesNotCreateIt(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "amux.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, exists, err := sqliteTableColumns(context.Background(), store.db, "execution_runs")
	if err != nil || exists {
		t.Fatalf("fresh startup created execution_runs: exists=%v err=%v", exists, err)
	}
}
