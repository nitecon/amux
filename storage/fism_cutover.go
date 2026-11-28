package storage

import (
	"context"
	"fmt"
	"sync"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/rs/zerolog/log"
)

// AcquireReconciliation reserves the store for one runtime without touching SQLite.
func (s *Store) AcquireReconciliation() (func(), error) {
	lease := s.routerLease()
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.active {
		return nil, fism.ErrRuntimeInactive
	}
	lease.active = true
	return sync.OnceFunc(func() {
		lease.mu.Lock()
		defer lease.mu.Unlock()
		lease.active = false
	}), nil
}

// RecoverLegacyInputs retires scheduler progress after preserving each valid
// accepted input once. The old rows only identify unfinished inputs. The
// active registry and current domain state govern each fresh pass.
func (s *Store) RecoverLegacyInputs(ctx context.Context, registry *fism.Registry) error {
	for {
		n, err := s.recoverLegacyInputBatch(ctx, registry)
		if err != nil {
			return fmt.Errorf("recover legacy scheduler inputs: %w", err)
		}
		if n == 0 {
			return nil
		}
	}
}

func (s *Store) recoverLegacyInputBatch(ctx context.Context, registry *fism.Registry) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT e.seq,e.uid,e.id,e.version,e.source,e.payload,e.emitted_at
		FROM router_events e JOIN (
			SELECT DISTINCT event_seq FROM router_deliveries
			WHERE state IN ('pending','running','retrying') ORDER BY event_seq LIMIT 200
		) d ON d.event_seq=e.seq ORDER BY e.seq`)
	if err != nil {
		return 0, err
	}
	var batch []fism.Event
	for rows.Next() {
		var e fism.Event
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.UID, &e.ID, &e.Version, &e.Source, &payload, &e.EmittedAt); err != nil {
			rows.Close()
			return 0, err
		}
		e.Payload = payload
		batch = append(batch, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, event := range batch {
		switch event.ID {
		case "clock.tick", controllers.RetentionStartupID, "router.retrying", controllers.GlobalNotifyEventID:
			// The clock and startup retention produce new signals. Retry notices
			// and global broadcasts are publication history only.
		default:
			if len(registry.Subscribers(event.ID, event.Version)) != 0 {
				if err := registry.Validate(event.ID, event.Version, event.Payload); err != nil {
					log.Warn().Ctx(ctx).Str("event_uid", event.UID).Str("event_id", event.ID).Msg("legacy input no longer matches its event definition")
				} else if _, err := tx.ExecContext(ctx, `INSERT INTO amux_heartbeat_outbox(uid,source,payload,created_at)
					VALUES(?,?,?,?) ON CONFLICT(uid) DO NOTHING`, event.UID, event.Source, string(event.Payload), event.EmittedAt); err != nil {
					return 0, err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM router_deliveries WHERE event_seq=?
			AND state IN ('pending','running','retrying')`, event.Seq); err != nil {
			return 0, err
		}
	}
	return len(batch), tx.Commit()
}
