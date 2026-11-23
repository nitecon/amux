package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestMigrateWorkerGoalsIsAdditiveAndIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy-worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE amux_go_migrations(version INTEGER PRIMARY KEY)"); err == nil {
		err = migrateWorker(ctx, tx)
	}
	if err == nil {
		err = migrateWorkerPool(ctx, tx)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO amux_worker_pools(id,server,project,directory,provider,os,command_json,slot_count) VALUES('pool','https://host','project','/tmp','codex','linux','[]',1)`); err == nil {
		_, err = db.ExecContext(ctx, `INSERT INTO amux_worker_runs(id,server,project,conversation_id,directory,command_json,owner_json) VALUES('run','https://host','project','conversation','/tmp','[]','{}')`)
	}
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = migrateWorkerGoals(ctx, tx); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"amux_worker_pools", "amux_worker_runs"} {
		found, err := sqliteTableHasColumn(ctx, db, table, "goal")
		if err != nil || !found {
			t.Fatalf("%s.goal found=%v err=%v", table, found, err)
		}
	}
	var migrations int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_go_migrations WHERE version=29").Scan(&migrations); err != nil || migrations != 1 {
		t.Fatalf("migration 29 rows=%d err=%v", migrations, err)
	}
	legacyPool, err := readWorkerPool(ctx, db, "pool")
	if err != nil || legacyPool.Goal != nil {
		t.Fatalf("legacy pool goal=%v err=%v", legacyPool.Goal, err)
	}
	legacyRun, err := readWorkerRun(ctx, db, "run")
	if err != nil || legacyRun.Goal != nil {
		t.Fatalf("legacy run goal=%v err=%v", legacyRun.Goal, err)
	}
}

func TestWorkerPoolAndRunPersistNormalizedImmutableGoal(t *testing.T) {
	ctx := context.Background()
	s, pool, path := workerPoolFixture(t, 1)
	selected := "  deploy   "
	pool.Goal = &selected
	created, err := s.CreateWorkerPool(ctx, pool)
	if err != nil || created.Goal == nil || *created.Goal != "deploy" {
		t.Fatalf("created pool goal=%v err=%v", created.Goal, err)
	}
	slot := onlyPoolSlot(t, s, pool.ID)
	runGoal := " deploy "
	run := WorkerRun{ID: uuid.NewString(), Server: pool.Server, Project: pool.Project, ConversationID: uuid.NewString(), Directory: pool.Directory, Command: append([]string(nil), pool.Command...), Owner: slot.Owner, Goal: &runGoal}
	if err := s.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
		t.Fatalf("attach matching normalized goal: %v", err)
	}
	saved, err := s.WorkerRun(ctx, run.ID)
	if err != nil || saved.Goal == nil || *saved.Goal != "deploy" {
		t.Fatalf("saved run goal=%v err=%v", saved.Goal, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loadedPool, err := reopened.WorkerPool(ctx, pool.ID)
	if err != nil || loadedPool.Goal == nil || *loadedPool.Goal != "deploy" {
		t.Fatalf("reopened pool goal=%v err=%v", loadedPool.Goal, err)
	}
	loadedRun, err := reopened.WorkerRun(ctx, run.ID)
	if err != nil || loadedRun.Goal == nil || *loadedRun.Goal != "deploy" {
		t.Fatalf("reopened run goal=%v err=%v", loadedRun.Goal, err)
	}
	changed := *loadedRun
	other := "review"
	changed.Goal = &other
	if err := reopened.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, changed); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("reattach changed goal: %v", err)
	}
}

func TestAttachWorkerPoolRunRejectsGoalMismatchWithoutSavingRun(t *testing.T) {
	ctx := context.Background()
	s, pool, _ := workerPoolFixture(t, 1)
	goal := "build"
	pool.Goal = &goal
	createWorkerPool(t, s, pool)
	slot := onlyPoolSlot(t, s, pool.ID)
	run := WorkerRun{ID: uuid.NewString(), Server: pool.Server, Project: pool.Project, ConversationID: uuid.NewString(), Directory: pool.Directory, Command: append([]string(nil), pool.Command...), Owner: slot.Owner}
	if err := s.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); !errors.Is(err, ErrWorkerPoolConflict) {
		t.Fatalf("missing run goal accepted: %v", err)
	}
	if _, err := s.WorkerRun(ctx, run.ID); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("mismatch persisted a run: %v", err)
	}
}
