package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

type LegacyReviewChange struct {
	TaskID string
	DoneAt int64
	Event  fism.Event
}

// PublishLegacyReviewChanges observes current completions and publishes their
// WebSocket records. The source remains durable until controller settlement.
func (s *Store) PublishLegacyReviewChanges(ctx context.Context, registry *fism.Registry, limit int) ([]LegacyReviewChange, int, error) {
	if limit < 1 || limit > 200 {
		return nil, 0, fmt.Errorf("legacy review outbox limit must be between 1 and 200")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT o.source_task_id,o.source_done_at,t.project_ident
		FROM task_review_outbox o LEFT JOIN tasks t ON t.id=o.source_task_id
		ORDER BY o.source_done_at,o.source_task_id LIMIT ?`, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read legacy review outbox: %w", err)
	}
	type obligation struct {
		taskID  string
		doneAt  int64
		project sql.NullString
	}
	batch := make([]obligation, 0, limit)
	for rows.Next() {
		var row obligation
		if err := rows.Scan(&row.taskID, &row.doneAt, &row.project); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("scan legacy review obligation: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("read legacy review obligations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close legacy review obligations: %w", err)
	}

	changes := make([]LegacyReviewChange, 0, len(batch))
	wake := false
	for _, row := range batch {
		var status string
		var taskType string
		var currentDone sql.NullInt64
		var project string
		var taskSnapshot []byte
		var current bool
		if row.project.Valid {
			err := tx.QueryRowContext(ctx, `SELECT t.project_ident,t.status,t.done_at,t.task_type,`+taskSnapshotSQL("t")+`
				FROM tasks t WHERE t.id=? AND t.project_ident=?`, row.taskID, row.project.String).
				Scan(&project, &status, &currentDone, &taskType, &taskSnapshot)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, 0, fmt.Errorf("read current completed task %q: %w", row.taskID, err)
			}
			current = err == nil && status == "done" && taskType == "software" && currentDone.Valid && currentDone.Int64 == row.doneAt
		}

		if current {
			if registry == nil || !registry.Subscribes("review", "task.completed", 1) {
				return nil, 0, fmt.Errorf("legacy review bridge requires the review controller to subscribe to task.completed v1")
			}
			payload, err := marshalRouterTaskPayload(routerTaskOutboxRow{
				project: project, task: taskSnapshot,
			})
			if err != nil {
				return nil, 0, fmt.Errorf("marshal current task %q for review bridge: %w", row.taskID, err)
			}
			uid := legacyReviewUID(row.taskID, row.doneAt)
			request := fism.EmitRequest{UID: uid, ID: "task.completed", Version: 1, Payload: payload, Source: "tasks"}
			event := fism.Event{UID: uid, ID: request.ID, Version: request.Version, Payload: payload, Source: request.Source}
			err = tx.QueryRowContext(ctx, "SELECT seq,emitted_at FROM router_events WHERE uid=? AND id=? AND version=? AND source=?",
				uid, request.ID, request.Version, request.Source).Scan(&event.Seq, &event.EmittedAt)
			if errors.Is(err, sql.ErrNoRows) {
				result, err := writeRouterEventIn(ctx, tx, registry, request, false)
				if err != nil {
					return nil, 0, fmt.Errorf("publish review for task %q: %w", row.taskID, err)
				}
				event = result.Event
				wake = wake || result.Stored
			} else if err != nil {
				return nil, 0, err
			}
			var settled bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM router_deliveries
    WHERE event_seq=? AND plugin='review' AND state IN ('completed','stopped','skipped','failed'))`, event.Seq).Scan(&settled); err != nil {
				return nil, 0, err
			}
			if !settled {
				// The sequence belongs to the broadcast. Controllers see a fresh
				// observation from the current completion revision.
				changes = append(changes, LegacyReviewChange{TaskID: row.taskID, DoneAt: row.doneAt, Event: event})
				continue
			}
		}
		if err := acknowledgeLegacyReview(ctx, tx, row.taskID, row.doneAt); err != nil {
			return nil, 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	if wake {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return changes, len(batch), nil
}

func legacyReviewUID(taskID string, doneAt int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("amux:legacy-review:%s:%d", taskID, doneAt))).String()
}

func (s *Store) SettleLegacyReviewChange(ctx context.Context, change LegacyReviewChange, outcomes []fism.Delivery) error {
	if change.Event.UID != legacyReviewUID(change.TaskID, change.DoneAt) {
		return fmt.Errorf("legacy review for task %q has a different event identity", change.TaskID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeControllerOutcomes(ctx, tx, change.Event, outcomes); err != nil {
		return err
	}
	if err := acknowledgeLegacyReview(ctx, tx, change.TaskID, change.DoneAt); err != nil {
		return err
	}
	return tx.Commit()
}

func acknowledgeLegacyReview(ctx context.Context, tx *sql.Tx, taskID string, doneAt int64) error {
	// Deleting the source task can cascade this row away during reconciliation.
	_, err := tx.ExecContext(ctx, "DELETE FROM task_review_outbox WHERE source_task_id=? AND source_done_at=?", taskID, doneAt)
	if err != nil {
		return fmt.Errorf("acknowledge legacy review for task %q: %w", taskID, err)
	}
	return nil
}
