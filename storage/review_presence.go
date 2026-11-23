package storage

import (
	"context"
	"database/sql"
)

func migrateReviewPresence(ctx context.Context, tx *sql.Tx) error {
	var present bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('amux_presence_agents') WHERE name='review_context_clear')").Scan(&present); err != nil {
		return err
	}
	if !present {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_presence_agents ADD COLUMN review_context_clear INTEGER NOT NULL DEFAULT 0 CHECK(review_context_clear IN(0,1))"); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(12)")
	return err
}
