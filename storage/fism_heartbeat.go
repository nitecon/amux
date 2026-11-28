package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

// AcceptedObservation is an accepted domain observation waiting for reconciliation.
// Its UID identifies the same durable observation across restarts.
type AcceptedObservation struct {
	ID    int64
	Event fism.Event
}

func (s *Store) PendingObservations(ctx context.Context, limit int) ([]AcceptedObservation, error) {
	return readObservations(ctx, s.db, 0, limit, "COALESCE(e.id,'') NOT IN (?,?)", controllers.NotifyEventID, controllers.InboundMessageReceivedID)
}

// PendingNotifications reads accepted notification snapshots in bounded pages.
func (s *Store) PendingNotifications(ctx context.Context, after int64, limit int) ([]AcceptedObservation, error) {
	return readObservations(ctx, s.db, after, limit, "e.id=?", controllers.NotifyEventID)
}

func (s *Store) PendingInboundMessages(ctx context.Context, limit int) ([]AcceptedObservation, error) {
	return readObservations(ctx, s.db, 0, limit, "e.id=?", controllers.InboundMessageReceivedID)
}

// ObservationWatermark bounds a notification sweep while producers remain active.
func (s *Store) ObservationWatermark(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM amux_heartbeat_outbox").Scan(&id)
	return id, err
}

// AcceptObservation saves the observation and its journal event identity atomically.
// The observation source processes inputs in acceptance order.
func (s *Store) AcceptObservation(ctx context.Context, registry *fism.Registry, request fism.EmitRequest) (fism.EmitResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fism.EmitResult{}, err
	}
	defer tx.Rollback()
	result, err := acceptObservationIn(ctx, tx, registry, request)
	if err != nil || !result.Stored {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return fism.EmitResult{}, err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.ObservationAccepted})
	return result, nil
}

func acceptObservationIn(ctx context.Context, tx *sql.Tx, registry *fism.Registry, request fism.EmitRequest) (fism.EmitResult, error) {
	result, err := writeRouterEventIn(ctx, tx, registry, request, false)
	if err != nil || !result.Stored {
		return result, err
	}
	event := result.Event
	if _, err := tx.ExecContext(ctx, `INSERT INTO amux_heartbeat_outbox(uid,source,payload,created_at) VALUES(?,?,?,?)`,
		event.UID, event.Source, string(event.Payload), event.EmittedAt); err != nil {
		return fism.EmitResult{}, fmt.Errorf("save accepted observation: %w", err)
	}
	return result, nil
}

func readObservations(ctx context.Context, q queryer, after int64, limit int, filter string, args ...any) ([]AcceptedObservation, error) {
	if limit < 1 || limit > 200 {
		return nil, fmt.Errorf("observation limit must be between 1 and 200")
	}
	// Raw observations use the existing event identity for their presence cursor.
	// The accepted payload always comes from the source row.
	rows, err := q.QueryContext(ctx, `SELECT h.id,h.uid,h.source,h.payload,h.created_at,
		COALESCE(e.id,'client.heartbeat'),COALESCE(e.version,1),COALESCE(e.seq,0),e.correlation
		FROM amux_heartbeat_outbox h LEFT JOIN router_events e ON e.uid=h.uid
		WHERE h.id>? AND `+filter+` ORDER BY h.id LIMIT ?`, append(append([]any{after}, args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var batch []AcceptedObservation
	for rows.Next() {
		var item AcceptedObservation
		event := &item.Event
		var payload []byte
		if err := rows.Scan(&item.ID, &event.UID, &event.Source, &payload, &event.EmittedAt,
			&event.ID, &event.Version, &event.Seq, &event.Correlation); err != nil {
			return nil, err
		}
		event.Payload = payload
		if !canonicalUUID(event.UID) || !validRouterSource(event.Source) || !json.Valid(event.Payload) {
			return nil, fmt.Errorf("invalid accepted observation %d", item.ID)
		}
		batch = append(batch, item)
	}
	return batch, rows.Err()
}

// SettleObservation acknowledges the exact observation after its
// controllers have settled their complete domain outcomes.
func (s *Store) SettleObservation(ctx context.Context, id int64, uid string) error {
	return settleObservation(ctx, s.db, id, uid)
}

func settleObservation(ctx context.Context, q queryer, id int64, uid string) error {
	result, err := q.ExecContext(ctx, "DELETE FROM amux_heartbeat_outbox WHERE id=? AND uid=?", id, uid)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("accepted observation %d changed before settlement", id)
	}
	return nil
}
