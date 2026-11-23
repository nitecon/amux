package storage

import (
	"context"
	"database/sql"
	"fmt"
)

func migratePresenceHostname(ctx context.Context, tx *sql.Tx) error {
	var found bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('amux_presence_agents') WHERE name='hostname')").Scan(&found); err != nil {
		return fmt.Errorf("inspect presence agent columns: %w", err)
	}
	if !found {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_presence_agents ADD COLUMN hostname TEXT"); err != nil {
			return fmt.Errorf("add presence hostname: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(18)"); err != nil {
		return fmt.Errorf("record presence hostname migration: %w", err)
	}
	return nil
}
