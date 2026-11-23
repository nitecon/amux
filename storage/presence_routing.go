package storage

import (
	"context"
	"database/sql"
	"fmt"
)

func migratePresenceRouting(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []string{"goal", "generation", "agent_id"} {
		found, err := sqliteTableHasColumn(ctx, tx, "amux_presence_agents", column)
		if err != nil {
			return fmt.Errorf("inspect presence routing column %s: %w", column, err)
		}
		if found {
			continue
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_presence_agents ADD COLUMN "+column+" TEXT"); err != nil {
			return fmt.Errorf("add presence routing column %s: %w", column, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(21)"); err != nil {
		return fmt.Errorf("record presence routing migration: %w", err)
	}
	return nil
}
