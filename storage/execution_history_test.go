package storage

import (
	"context"
	"encoding/json"
	"testing"
)

func TestListExecutionRunsLegacySchemaReadOnlyAndOrdered(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, fixtureProject("history")); err != nil {
		t.Fatal(err)
	}
	task, err := s.InsertTask(ctx, TaskInsert{Project: "history", Title: "Retained task", Reporter: "tester"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.ExecContext(ctx, `CREATE TABLE execution_runs (
id TEXT PRIMARY KEY, project_ident TEXT NOT NULL, task_id TEXT, trigger TEXT NOT NULL,
dedup_key TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL,
finished_at INTEGER, output TEXT NOT NULL DEFAULT '');
CREATE TABLE execution_attempts (run_id TEXT NOT NULL, position INTEGER NOT NULL, body TEXT NOT NULL, PRIMARY KEY(run_id,position));
INSERT INTO execution_runs VALUES('unfinished','history',?,'manual','unfinished-key','running',20,NULL,'partial output');
INSERT INTO execution_runs VALUES('finished','history',?,'manual','finished-key','completed',10,30,'done output');
INSERT INTO execution_attempts VALUES('unfinished',2,'{"position":2}');
INSERT INTO execution_attempts VALUES('unfinished',1,'{"position":1}');`, task.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	project := "history"
	runs, err := s.ListExecutionRuns(ctx, &project, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "unfinished" || runs[1].ID != "finished" {
		t.Fatalf("legacy ordering: %+v", runs)
	}
	legacy := runs[0]
	if legacy.TaskTitle == nil || *legacy.TaskTitle != "Retained task" || legacy.StartedAt != nil || legacy.UpdatedAt != 20 || legacy.Executor != "headless" || legacy.Progress != "" || legacy.Summary != "" || legacy.SummarySource != "missing" || legacy.LastSequence != 0 {
		t.Fatalf("legacy defaults/title: %+v", legacy)
	}
	if len(legacy.Attempts) != 2 || !json.Valid(legacy.Attempts[0]) || string(legacy.Attempts[0]) != `{"position":1}` || string(legacy.Attempts[1]) != `{"position":2}` {
		t.Fatalf("attempt order/content: %s", legacy.Attempts)
	}
	finished := runs[1]
	if finished.UpdatedAt != 30 || finished.FinishedAt == nil || *finished.FinishedAt != 30 {
		t.Fatalf("legacy updated_at fallback should use finished_at: %+v", finished)
	}
	emptyProject := ""
	emptyRuns, err := s.ListExecutionRuns(ctx, &emptyProject, nil)
	if err != nil || len(emptyRuns) != 0 {
		t.Fatalf("empty project filter must remain a filter: runs=%+v err=%v", emptyRuns, err)
	}
	columns, exists, err := sqliteTableColumns(ctx, s.db, "execution_runs")
	if err != nil || !exists || columns["started_at"] || columns["updated_at"] {
		t.Fatalf("read added columns or failed table check: exists=%v columns=%v err=%v", exists, columns, err)
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM execution_runs WHERE id='unfinished'`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("history read mutated legacy run status=%q err=%v", status, err)
	}
}

func TestListExecutionRunsFreshStoreIsEmptyAndReadOnly(t *testing.T) {
	s := testStore(t)
	runs, err := s.ListExecutionRuns(context.Background(), nil, nil)
	if err != nil || len(runs) != 0 {
		t.Fatalf("fresh store history: %+v err=%v", runs, err)
	}
	_, exists, err := sqliteTableColumns(context.Background(), s.db, "execution_runs")
	if err != nil || exists {
		t.Fatalf("fresh history read created execution_runs: exists=%v err=%v", exists, err)
	}
}
