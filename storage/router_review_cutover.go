package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

// DrainLegacyReviewOutbox bridges obligations left by the legacy synchronous
// review path into durable task.completed events. Emission and acknowledgement
// share one transaction so a failed append leaves every obligation retryable.
func (s *Store) DrainLegacyReviewOutbox(ctx context.Context, registry *router.Registry, limit int) (int, error) {
	if limit < 1 || limit > 200 {
		return 0, fmt.Errorf("legacy review outbox limit must be between 1 and 200")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT o.source_task_id,o.source_done_at,t.project_ident
		FROM task_review_outbox o LEFT JOIN tasks t ON t.id=o.source_task_id
		ORDER BY o.source_done_at,o.source_task_id LIMIT ?`, limit)
	if err != nil {
		return 0, fmt.Errorf("read legacy review outbox: %w", err)
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
			return 0, fmt.Errorf("scan legacy review obligation: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read legacy review obligations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close legacy review obligations: %w", err)
	}

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
				return 0, fmt.Errorf("read current completed task %q: %w", row.taskID, err)
			}
			current = err == nil && status == "done" && taskType == "software" && currentDone.Valid && currentDone.Int64 == row.doneAt
		}

		if current {
			if registry == nil || !registry.Subscribes("review", "task.completed", 1) {
				return 0, fmt.Errorf("legacy review bridge requires the review plugin to subscribe to task.completed v1")
			}
			payload, err := marshalRouterTaskPayload(routerTaskOutboxRow{
				project: project, task: taskSnapshot,
			})
			if err != nil {
				return 0, fmt.Errorf("marshal current task %q for review bridge: %w", row.taskID, err)
			}
			uid := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("amux:legacy-review:%s:%d", row.taskID, row.doneAt))).String()
			result, err := emitRouterEventIn(ctx, tx, registry, router.EmitRequest{
				UID: uid, ID: "task.completed", Version: 1, Payload: payload, Source: "tasks",
			})
			if err != nil {
				return 0, fmt.Errorf("emit legacy review bridge for task %q: %w", row.taskID, err)
			}
			if !result.Stored && !result.Replayed {
				return 0, fmt.Errorf("legacy review bridge for task %q was not durably stored", row.taskID)
			}
			wake = true
		}

		deleted, err := tx.ExecContext(ctx, "DELETE FROM task_review_outbox WHERE source_task_id=? AND source_done_at=?", row.taskID, row.doneAt)
		if err != nil {
			return 0, fmt.Errorf("acknowledge legacy review obligation for task %q: %w", row.taskID, err)
		}
		count, err := deleted.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count acknowledged legacy review obligation: %w", err)
		}
		if count != 1 {
			return 0, fmt.Errorf("acknowledged %d legacy review obligations for task %q; expected 1", count, row.taskID)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if wake {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return len(batch), nil
}
