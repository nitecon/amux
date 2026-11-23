package storage

import (
	"context"
	"database/sql"
	"fmt"
)

const workerPresenceMigration = 33

func migrateWorkerPresence(ctx context.Context, tx *sql.Tx) error {
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "amux_worker_runs", "presence_sequence")
	if err != nil {
		return fmt.Errorf("inspect worker presence sequence: %w", err)
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE amux_worker_runs ADD COLUMN presence_sequence INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("add worker presence sequence: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", workerPresenceMigration); err != nil {
		return fmt.Errorf("record worker presence migration: %w", err)
	}
	return nil
}

// NextWorkerRunReport allocates one durable sequence for a new logical report.
// Keep the returned snapshot unchanged while retrying its HTTP delivery.
func (s *Store) NextWorkerRunReport(ctx context.Context, id string) (*WorkerRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := readWorkerRun(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if r.PresenceSequence == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("worker presence sequence exhausted")
	}
	r.PresenceSequence++
	if _, err = tx.ExecContext(ctx, "UPDATE amux_worker_runs SET presence_sequence=? WHERE id=?", r.PresenceSequence, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}
