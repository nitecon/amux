package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/nitecon/amux/fism"
)

// The task outbox ID is durable and never reused within the database. This
// namespace keeps the corresponding WebSocket event stable across restarts.
const taskChangeUIDFormat = "9e7ae42a-%04x-5000-8000-%012x"
const taskChangeUIDSQL = "printf('" + taskChangeUIDFormat + "', t.id >> 48, t.id & 281474976710655)"

func taskChangeUID(id int64) string {
	return fmt.Sprintf(taskChangeUIDFormat, id>>48, id&0xffffffffffff)
}

type TaskChange struct {
	ID      int64
	Request fism.EmitRequest
}

func (s *Store) PendingTaskChanges(ctx context.Context, limit int) ([]TaskChange, error) {
	rows, err := readTaskOutbox(ctx, s.db, limit)
	if err != nil {
		return nil, err
	}
	changes := make([]TaskChange, 0, len(rows))
	for _, row := range rows {
		request, err := taskChangeRequest(row)
		if err != nil {
			return nil, err
		}
		request.UID = taskChangeUID(row.id)
		changes = append(changes, TaskChange{ID: row.id, Request: request})
	}
	return changes, nil
}

// PublishTaskChange records the WebSocket event without writing controller
// progress. A restarted pass uses the same event identity for domain deduplication.
func (s *Store) PublishTaskChange(ctx context.Context, registry *fism.Registry, change TaskChange) (fism.Event, error) {
	result, err := s.PublishBroadcast(ctx, registry, change.Request)
	return result.Event, err
}

// SettleTaskChange commits the complete controller outcomes and removes the
// accepted task snapshot in the same transaction.
func (s *Store) SettleTaskChange(ctx context.Context, id int64, event fism.Event, outcomes []fism.Delivery) error {
	if id <= 0 || event.UID != taskChangeUID(id) {
		return fmt.Errorf("task change %d has a different event identity", id)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := writeControllerOutcomes(ctx, tx, event, outcomes); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM router_task_outbox WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("task change %d changed before settlement", id)
	}
	return tx.Commit()
}

func writeControllerOutcomes(ctx context.Context, tx *sql.Tx, event fism.Event, outcomes []fism.Delivery) error {
	if event.Seq != 0 {
		var uid string
		if err := tx.QueryRowContext(ctx, "SELECT uid FROM router_events WHERE seq=?", event.Seq).Scan(&uid); err != nil {
			return err
		}
		if uid != event.UID {
			return fmt.Errorf("event %s broadcast changed before settlement", event.UID)
		}
	}
	for _, outcome := range outcomes {
		switch outcome.State {
		case "completed", "stopped", "skipped", "failed":
		default:
			return fmt.Errorf("cannot persist incomplete controller outcome %q", outcome.State)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO router_deliveries
			(event_seq,plugin,state,attempts,next_attempt_at,last_error,updated_at)
			VALUES(?,?,?,?,0,?,?)`, event.Seq, outcome.Plugin, outcome.State, outcome.Attempts,
			outcome.LastError, outcome.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}
