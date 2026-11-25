package storage

import "context"

// UpgradeLoopbackWorkerOrigins preserves worker identities and active journals
// when the same local gateway upgrades from HTTP to enrolled TLS on port 7913.
// The caller holds the worker journal lock and has selected the local manifest.
func (s *Store) UpgradeLoopbackWorkerOrigins(ctx context.Context) error {
	const oldOrigin = "http://127.0.0.1:7913"
	const newOrigin = "https://127.0.0.1:7913"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"UPDATE amux_worker_pools SET server=? WHERE server=?",
		"UPDATE amux_worker_runs SET server=? WHERE server=?",
		"UPDATE OR IGNORE amux_worker_task_inbox SET server=? WHERE server=?",
	} {
		if _, err := tx.ExecContext(ctx, statement, newOrigin, oldOrigin); err != nil {
			return err
		}
	}
	// The enrolled client bootstraps a new subscription using current
	// ownership. The old transport's consumer identity stays in its namespace.
	return tx.Commit()
}
