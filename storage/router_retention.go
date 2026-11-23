package storage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

const (
	retentionDayMS      int64 = 86_400_000
	retentionReplayMS   int64 = 86_400_000
	retentionPruneBatch       = 256
)

func migrateRouterRetention(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS retention_runs (
		id INTEGER PRIMARY KEY CHECK(id=1),
		completed_at INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS remote_consumers (
		consumer_id TEXT PRIMARY KEY,
		connection_id TEXT,
		cursor INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_remote_consumers_cursor ON remote_consumers(cursor);
	INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(24);`); err != nil {
		return fmt.Errorf("migrate router retention: %w", err)
	}
	return nil
}

// ReconcileRouterRetention runs startup cleanup immediately and clock cleanup
// no more than once per day. All cleanup and its checkpoint share one tx.
func (s *Store) ReconcileRouterRetention(ctx context.Context, event router.Event, retentionDays, botRetentionDays uint64) (ran bool, messagesDeleted int64, err error) {
	if event.Version != 1 || (event.ID != "retention.startup" && event.ID != "clock.tick") || !canonicalUUID(event.UID) {
		return false, 0, fmt.Errorf("retention reconciliation requires a retention.startup or clock.tick v1 event with a canonical UID")
	}
	lease := s.routerLease()
	lease.mu.Lock()
	defer lease.mu.Unlock()
	heldIDs := make([]int64, 0, len(lease.activeClaims))
	seen := make(map[int64]struct{}, len(lease.activeClaims))
	for key := range lease.activeClaims {
		if _, exists := seen[key.seq]; exists {
			continue
		}
		seen[key.seq] = struct{}{}
		heldIDs = append(heldIDs, key.seq)
	}
	sort.Slice(heldIDs, func(i, j int) bool { return heldIDs[i] < heldIDs[j] })
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback()

	var previous sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT completed_at FROM retention_runs WHERE id=1").Scan(&previous)
	if err != nil && err != sql.ErrNoRows {
		return false, 0, fmt.Errorf("read retention checkpoint: %w", err)
	}
	if previous.Valid && (event.EmittedAt <= previous.Int64 || (event.ID == "clock.tick" && !retentionDayElapsed(previous.Int64, event.EmittedAt))) {
		if err := tx.Commit(); err != nil {
			return false, 0, err
		}
		return false, 0, nil
	}

	cutoff := event.EmittedAt - retentionDayMS
	if event.EmittedAt < -1<<63+retentionDayMS {
		cutoff = -1 << 63
	}
	throughSeq, err := remoteRetentionCursorIn(ctx, tx, event.EmittedAt)
	if err != nil {
		return false, 0, err
	}
	routerDeleted, err := pruneRouterRetentionIn(ctx, tx, cutoff, throughSeq, heldIDs)
	if err != nil {
		return false, 0, err
	}
	if err := pruneNativeDeliveryReceiptsIn(ctx, tx, cutoff); err != nil {
		return false, 0, err
	}
	mainCutoff := retentionCutoff(event.EmittedAt, retentionDays)
	botCutoff := retentionCutoff(event.EmittedAt, botRetentionDays)
	messagesDeleted, err = purgeRetentionMessagesIn(ctx, tx, mainCutoff, botCutoff)
	if err != nil {
		return false, 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO retention_runs(id,completed_at) VALUES(1,?)
		ON CONFLICT(id) DO UPDATE SET completed_at=excluded.completed_at`, event.EmittedAt); err != nil {
		return false, 0, fmt.Errorf("write retention checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	if routerDeleted > 0 {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return true, messagesDeleted, nil
}

func retentionDayElapsed(previous, now int64) bool {
	if previous > (1<<63-1)-retentionDayMS {
		return false
	}
	return now >= previous+retentionDayMS
}

func retentionCutoff(now int64, days uint64) int64 {
	maxInt := int64(1<<63 - 1)
	if days > uint64(maxInt/retentionDayMS) {
		if now < 0 {
			return -1 << 63
		}
		return now - maxInt
	}
	duration := int64(days) * retentionDayMS
	if now < (-1<<63)+duration {
		return -1 << 63
	}
	return now - duration
}

func remoteRetentionCursorIn(ctx context.Context, tx *sql.Tx, now int64) (*int64, error) {
	cutoff := now - retentionReplayMS
	if now < (-1<<63)+retentionReplayMS {
		cutoff = -1 << 63
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM remote_consumers WHERE last_seen_at<=?", cutoff); err != nil {
		return nil, fmt.Errorf("expire remote retention consumers: %w", err)
	}
	var cursor sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT MIN(cursor) FROM remote_consumers").Scan(&cursor); err != nil {
		return nil, fmt.Errorf("read remote retention cursor: %w", err)
	}
	if !cursor.Valid {
		return nil, nil
	}
	return &cursor.Int64, nil
}

// pruneRouterRetentionIn removes only old events with terminal deliveries and
// no remaining remote or native outbox obligation.
func pruneRouterRetentionIn(ctx context.Context, tx *sql.Tx, cutoffMS int64, throughSeq *int64, heldIDs []int64) (int64, error) {
	var deleted int64
	var afterSeq int64
	for {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		query := `SELECT e.seq FROM router_events e
			WHERE e.seq>? AND e.emitted_at<? AND (? IS NULL OR e.seq<=?)
			AND NOT EXISTS (SELECT 1 FROM router_deliveries d WHERE d.event_seq=e.seq
				AND d.state NOT IN ('completed','stopped','skipped','failed'))
			AND NOT EXISTS (SELECT 1 FROM amux_presence_outbox p WHERE p.correlation=e.uid)
			AND NOT EXISTS (SELECT 1 FROM amux_heartbeat_outbox h WHERE h.uid=e.uid)
			`
		args := []any{afterSeq, cutoffMS, throughSeq, throughSeq}
		if len(heldIDs) != 0 {
			marks, ids := retentionIDArgs(heldIDs)
			query += " AND e.seq NOT IN (" + marks + ")"
			args = append(args, ids...)
		}
		query += " ORDER BY e.seq LIMIT ?"
		args = append(args, retentionPruneBatch)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return deleted, fmt.Errorf("select router retention candidates: %w", err)
		}
		ids := make([]int64, 0, retentionPruneBatch)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return deleted, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return deleted, err
		}
		if err := rows.Close(); err != nil {
			return deleted, err
		}
		if len(ids) == 0 {
			return deleted, nil
		}
		afterSeq = ids[len(ids)-1]
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		marks, args := retentionIDArgs(ids)
		result, err := tx.ExecContext(ctx, "DELETE FROM router_events WHERE seq IN ("+marks+")", args...)
		if err != nil {
			return deleted, fmt.Errorf("delete router retention candidates: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, err
		}
		if count != int64(len(ids)) {
			return deleted, fmt.Errorf("deleted %d of %d router retention candidates", count, len(ids))
		}
		deleted += count
		if len(ids) < retentionPruneBatch {
			return deleted, nil
		}
	}
}
