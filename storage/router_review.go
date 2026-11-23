package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nitecon/amux/router"
)

// ReconcileRouterReviews queues reviews for the completion represented by a
// durable task.completed event. Invalid or obsolete task snapshots are benign.
func (s *Store) ReconcileRouterReviews(ctx context.Context, event router.Event) (bool, error) {
	if event.ID != "task.completed" || event.Version != 1 {
		return false, fmt.Errorf("review reconciliation requires a task.completed v1 event")
	}
	var payload struct {
		Project string `json:"project"`
		Task    *struct {
			ID     string          `json:"id"`
			DoneAt json.RawMessage `json:"done_at"`
		} `json:"task"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return false, nil
	}
	project := strings.ToLower(strings.TrimSpace(payload.Project))
	if project == "" || payload.Task == nil || strings.TrimSpace(payload.Task.ID) == "" {
		return false, nil
	}
	var snapshotDoneAt *int64
	if len(payload.Task.DoneAt) != 0 {
		if string(payload.Task.DoneAt) == "null" {
			return false, nil
		}
		var doneAt int64
		if err := json.Unmarshal(payload.Task.DoneAt, &doneAt); err != nil {
			return false, nil
		}
		snapshotDoneAt = &doneAt
	}
	processed := false
	err := s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *AgentOrigin) error {
		enabled, err := automaticReviewsEnabled(ctx, tx)
		if err != nil {
			return err
		}
		if !enabled {
			return nil
		}
		source, err := readTask(ctx, tx, project, payload.Task.ID)
		if err != nil {
			return err
		}
		if source == nil || source.Status != "done" || source.TaskType != "software" || source.DoneAt == nil {
			return nil
		}
		if snapshotDoneAt != nil && *source.DoneAt != *snapshotDoneAt {
			return nil
		}
		if _, err := enqueueTaskReviews(ctx, tx, source); err != nil {
			return err
		}
		processed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return processed, nil
}
