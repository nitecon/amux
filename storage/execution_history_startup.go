package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// retireLegacyExecutionRuns applies the retired executor's startup behavior to
// retained history. Fresh profiles without that table remain untouched.
func retireLegacyExecutionRuns(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	columns, exists, err := sqliteTableColumns(ctx, tx, "execution_runs")
	if err != nil {
		return err
	}
	if !exists {
		return tx.Commit()
	}
	for _, column := range []struct{ name, definition string }{
		{"started_at", "INTEGER"},
		{"updated_at", "INTEGER"},
		{"client", "TEXT"},
		{"model", "TEXT"},
		{"executor", "TEXT NOT NULL DEFAULT 'headless'"},
		{"progress", "TEXT NOT NULL DEFAULT ''"},
		{"summary", "TEXT NOT NULL DEFAULT ''"},
		{"summary_source", "TEXT NOT NULL DEFAULT 'missing'"},
		{"session_key", "TEXT"},
		{"last_sequence", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if columns[column.name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE execution_runs ADD COLUMN %s %s", column.name, column.definition)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_runs
		SET updated_at=COALESCE(updated_at,finished_at,created_at)
		WHERE updated_at IS NULL`); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE execution_runs
		SET status='retired',finished_at=?,updated_at=?,progress='Legacy executor removed; no work resumed'
		WHERE finished_at IS NULL`, now, now); err != nil {
		return err
	}
	return tx.Commit()
}
