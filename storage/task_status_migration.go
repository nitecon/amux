package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const currentTaskStatusCheck = `CHECK(status IN ('todo','in_progress','user_review','done','cancelled'))`

var taskStatusCheckPattern = regexp.MustCompile(`(?i:CHECK\s*\(\s*status\s+IN\s*\()\s*('todo'\s*,\s*'in_progress'\s*,\s*'done'(?:\s*,\s*'cancelled')?)\s*\)\s*\)`)
var currentTaskStatusPattern = regexp.MustCompile(`(?i:CHECK\s*\(\s*status\s+IN\s*\()\s*('todo'\s*,\s*'in_progress'\s*,\s*'user_review'\s*,\s*'done'\s*,\s*'cancelled')\s*\)\s*\)`)
var statusConstraintPattern = regexp.MustCompile(`(?i)CHECK\s*\(\s*status\b`)

// migrateLegacyTaskStatus widens only the two legacy tasks.status constraints
// recognized by the Rust gateway. It leaves a restrictive VACUUM INTO snapshot
// beside the database before rebuilding. The caller must invoke it before
// Store.migrate.
func migrateLegacyTaskStatus(ctx context.Context, db *sql.DB, path string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get SQLite connection: %w", err)
	}
	defer conn.Close()

	var schema sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='tasks'").Scan(&schema); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("read tasks schema: %w", err)
	}
	if !schema.Valid {
		return fmt.Errorf("tasks table has no stored schema")
	}

	ddl := schema.String
	if current := currentTaskStatusPattern.FindStringSubmatch(ddl); current != nil && compactSQL(current[1]) == compactSQL("'todo','in_progress','user_review','done','cancelled'") {
		if len(statusConstraintPattern.FindAllStringIndex(ddl, -1)) != 1 {
			return fmt.Errorf("unrecognized additional tasks status constraint; refusing task status migration")
		}
		return nil
	}
	match := taskStatusCheckPattern.FindStringSubmatchIndex(ddl)
	if match == nil {
		return fmt.Errorf("unrecognized tasks status constraint; refusing task status migration")
	}
	if len(statusConstraintPattern.FindAllStringIndex(ddl, -1)) != 1 {
		return fmt.Errorf("unrecognized additional tasks status constraint; refusing task status migration")
	}
	values := compactSQL(ddl[match[2]:match[3]])
	if values != compactSQL("'todo','in_progress','done'") && values != compactSQL("'todo','in_progress','done','cancelled'") {
		return fmt.Errorf("unrecognized tasks status constraint; refusing task status migration")
	}

	_, _, err = createTaskStatusBackup(ctx, conn, path)
	if err != nil {
		return err
	}

	newDDL := ddl[:match[0]] + currentTaskStatusCheck + ddl[match[1]:]
	newDDL, err = replaceCreateTableName(newDDL, "tasks", "tasks_status_rebuild")
	if err != nil {
		return err
	}
	return rebuildLegacyTable(ctx, conn, "tasks", "tasks_status_rebuild", newDDL)
}

func rebuildLegacyTable(ctx context.Context, conn *sql.Conn, table, rebuildTable, newDDL string) (retErr error) {
	var foreignKeys, legacyAlter int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA legacy_alter_table").Scan(&legacyAlter); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if restoreErr := restoreLegacyTablePragmas(cleanupCtx, conn, foreignKeys, legacyAlter); restoreErr != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			retErr = errors.Join(retErr, restoreErr, fmt.Errorf("discarded SQLite connection after PRAGMA restoration failure"))
		}
	}()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disable foreign keys for %s rebuild: %w", table, err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA legacy_alter_table=ON"); err != nil {
		return fmt.Errorf("set legacy alter behavior: %w", err)
	}
	var off int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&off); err != nil {
		return err
	}
	if off != 0 {
		return fmt.Errorf("foreign keys must be off to rebuild %s", table)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	objects, err := legacyTableSchemaObjects(ctx, tx, table)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, newDDL); err != nil {
		return fmt.Errorf("create widened %s table: %w", table, err)
	}
	columns, err := legacyTableInsertColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	columnList := strings.Join(columns, ",")
	insert := "INSERT INTO " + quoteIdentifier(rebuildTable) + " (" + columnList + ") SELECT " + columnList + " FROM " + quoteIdentifier(table)
	if !strings.Contains(strings.ToUpper(newDDL), "WITHOUT ROWID") {
		insert = "INSERT INTO " + quoteIdentifier(rebuildTable) + "(rowid," + columnList + ") SELECT rowid," + columnList + " FROM " + quoteIdentifier(table)
	}
	if _, err := tx.ExecContext(ctx, insert); err != nil {
		return fmt.Errorf("copy %s rows: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE "+quoteIdentifier(table)); err != nil {
		return fmt.Errorf("drop legacy %s table: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, "ALTER TABLE "+quoteIdentifier(rebuildTable)+" RENAME TO "+quoteIdentifier(table)); err != nil {
		return fmt.Errorf("rename widened %s table: %w", table, err)
	}
	for _, object := range objects {
		if _, err := tx.ExecContext(ctx, object.sql); err != nil {
			return fmt.Errorf("restore %s %s %q: %w", table, object.kind, object.name, err)
		}
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("check foreign keys after %s rebuild: %w", table, err)
	}
	if rows.Next() {
		var violatingTable string
		var rowid, parent, fk any
		if err := rows.Scan(&violatingTable, &rowid, &parent, &fk); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		return fmt.Errorf("%s rebuild would leave foreign key violation in %s", table, violatingTable)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit %s rebuild: %w", table, err)
	}
	committed = true
	return nil
}

func createTaskStatusBackup(ctx context.Context, conn *sql.Conn, path string) (string, string, error) {
	dir := filepath.Dir(path)
	backupDir, err := os.MkdirTemp(dir, ".task-status-backup-")
	if err != nil {
		return "", "", fmt.Errorf("create protected task status backup directory: %w", err)
	}
	if err := os.Chmod(backupDir, 0700); err != nil {
		_ = os.RemoveAll(backupDir)
		return "", "", fmt.Errorf("protect task status backup directory: %w", err)
	}
	if err := syncTaskStatusBackupDirectory(dir); err != nil {
		return backupDir, "", fmt.Errorf("sync task status backup parent: %w", err)
	}
	backup := filepath.Join(backupDir, "database.sqlite")
	if _, err := conn.ExecContext(ctx, "VACUUM INTO ?", filepath.ToSlash(backup)); err != nil {
		_ = os.RemoveAll(backupDir)
		return "", "", fmt.Errorf("create task status backup: %w", err)
	}
	if err := os.Chmod(backup, 0600); err != nil {
		return backupDir, backup, fmt.Errorf("protect task status backup: %w", err)
	}
	f, err := os.OpenFile(backup, os.O_RDWR, 0)
	if err != nil {
		return backupDir, backup, fmt.Errorf("open task status backup for sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return backupDir, backup, fmt.Errorf("sync task status backup: %w", err)
	}
	if err := f.Close(); err != nil {
		return backupDir, backup, fmt.Errorf("close task status backup: %w", err)
	}
	if err := syncTaskStatusBackupDirectory(backupDir); err != nil {
		return backupDir, backup, fmt.Errorf("sync task status backup directory: %w", err)
	}
	return backupDir, backup, nil
}

type taskStatusSchemaObject struct{ kind, name, sql string }

func legacyTableSchemaObjects(ctx context.Context, tx *sql.Tx, table string) ([]taskStatusSchemaObject, error) {
	rows, err := tx.QueryContext(ctx, `SELECT type,name,sql FROM sqlite_master
		WHERE tbl_name=? AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []taskStatusSchemaObject
	for rows.Next() {
		var object taskStatusSchemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.sql); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func legacyTableInsertColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_xinfo("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notnull, pk, hidden int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk, &hidden); err != nil {
			return nil, err
		}
		if hidden == 0 {
			columns = append(columns, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("%s table has no copyable columns", table)
	}
	return columns, nil
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func compactSQL(sql string) string { return strings.Join(strings.Fields(sql), "") }

func replaceCreateTableName(ddl, from, to string) (string, error) {
	pattern := regexp.MustCompile(`(?i)^(\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?)(?:"` + from + `"|` + from + `|\[` + from + `\]|\x60` + from + `\x60)(\s|\()`)
	loc := pattern.FindStringSubmatchIndex(ddl)
	if loc == nil {
		return "", fmt.Errorf("unrecognized tasks CREATE TABLE header; refusing rebuild")
	}
	return ddl[:loc[3]] + to + ddl[loc[4]:], nil
}

func restoreLegacyTablePragmas(ctx context.Context, conn *sql.Conn, foreignKeys, legacyAlter int) error {
	var errs []error
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA foreign_keys=%d", foreignKeys)); err != nil {
		errs = append(errs, fmt.Errorf("restore foreign_keys: %w", err))
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA legacy_alter_table=%d", legacyAlter)); err != nil {
		errs = append(errs, fmt.Errorf("restore legacy_alter_table: %w", err))
	}
	var gotFK, gotLegacy int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&gotFK); err != nil {
		errs = append(errs, fmt.Errorf("verify foreign_keys restoration: %w", err))
	} else if gotFK != foreignKeys {
		errs = append(errs, fmt.Errorf("foreign_keys restoration mismatch: got %d, want %d", gotFK, foreignKeys))
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA legacy_alter_table").Scan(&gotLegacy); err != nil {
		errs = append(errs, fmt.Errorf("verify legacy_alter_table restoration: %w", err))
	} else if gotLegacy != legacyAlter {
		errs = append(errs, fmt.Errorf("legacy_alter_table restoration mismatch: got %d, want %d", gotLegacy, legacyAlter))
	}
	return errors.Join(errs...)
}
