package storage

import (
	"context"
	"database/sql"
)

// migrateWorkerGoals adds the optional, immutable goal scope to saved pools
// and their recovery runs. Existing rows remain unscoped (NULL).
func migrateWorkerGoals(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"amux_worker_pools", "amux_worker_runs"} {
		found, err := sqliteTableHasColumn(ctx, tx, table, "goal")
		if err != nil {
			return err
		}
		if !found {
			if _, err = tx.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN goal TEXT"); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(29)")
	return err
}

func optionalSQLString(goal sql.NullString) *string {
	if !goal.Valid {
		return nil
	}
	value := goal.String
	return &value
}
