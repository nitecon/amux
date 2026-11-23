package storage

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCreateTaskStatusBackupLeavesProtectedSnapshot(t *testing.T) {
	path, db := legacyStatusFixture(t, `CHECK(status IN ('todo','in_progress','done'))`)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	backupDir, backupPath, err := createTaskStatusBackup(context.Background(), conn, path)
	if err != nil {
		t.Fatal(err)
	}
	if backupPath != filepath.Join(backupDir, "database.sqlite") {
		t.Fatalf("backup path %q is outside backup directory %q", backupPath, backupDir)
	}
	info, err := os.Stat(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode %o, want 600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("backup directory mode %o, want 700", dirInfo.Mode().Perm())
	}
	backup, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	assertEqual(t, backup, "SELECT title FROM tasks WHERE id='t1'", "kept title")
	assertSchemaContains(t, backup, `CHECK(status IN ('todo','in_progress','done'))`)
}

func TestTaskStatusBackupFailureLeavesOriginalSchemaAndRows(t *testing.T) {
	path, db := legacyStatusFixture(t, `CHECK(status IN ('todo','in_progress','done'))`)
	blockedParent := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	blockedDBPath := filepath.Join(blockedParent, "tasks.sqlite")
	if err := migrateLegacyTaskStatus(context.Background(), db, blockedDBPath); err == nil || !strings.Contains(err.Error(), "create protected task status backup directory") {
		t.Fatalf("expected backup creation failure, got %v", err)
	}
	assertEqual(t, db, "SELECT title FROM tasks WHERE id='t1'", "kept title")
	assertSchemaContains(t, db, `CHECK(status IN ('todo','in_progress','done'))`)
	assertNoStatusBackup(t, path)
}
