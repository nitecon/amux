package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

// ReorderTasks applies a client supplied order to one status column. Rank-only
// updates preserve reservations and review state; column crossings use the
// existing task workflow in the same mutation transaction.
func (s *Store) ReorderTasks(ctx context.Context, project, status string, order []string, origin *AgentOrigin, policy TaskTransitionPolicy) ([]TaskSummary, error) {
	if !IsReorderableTaskStatus(status) {
		return nil, fmt.Errorf("%w: invalid task status", ErrTaskConflict)
	}
	project = projects.NormalizeIdent(project)
	if policy.HumanRequest && origin != nil {
		return nil, fmt.Errorf("%w: user controls require a human task request", ErrTaskConflict)
	}
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		crossing := false
		for i, id := range order {
			t, err := readTask(ctx, tx, project, id)
			if err != nil {
				return err
			}
			if t == nil {
				return ErrTaskNotFound
			}
			if err := ordinaryWorkflowTask(ctx, tx, t); err != nil {
				return err
			}
			if actor != nil && status == "in_progress" && t.Goal != nil && strings.TrimSpace(policy.SelectedGoal) != *t.Goal {
				return fmt.Errorf("%w: select the task's goal with X-Task-Goal before claiming", ErrTaskConflict)
			}
			if t.Status == status {
				if _, err := tx.ExecContext(ctx, "UPDATE tasks SET rank=?,updated_at=? WHERE id=? AND project_ident=?", i, time.Now().UnixMilli(), t.ID, project); err != nil {
					return err
				}
				continue
			}
			crossing = crossing || actor != nil
			if status == "stalled" {
				return fmt.Errorf("%w: stalled is managed by the activity controller", ErrTaskConflict)
			}
			if status == "user_review" {
				return fmt.Errorf("%w: enter user review with PATCH status=user_review and a specific review_action before reordering", ErrTaskConflict)
			}
			update := TaskWorkflowUpdate{
				Status: status,
				Policy: TaskTransitionPolicy{HumanRequest: actor == nil},
			}
			if actor != nil && status == "in_progress" {
				update.Policy.CheckGoal = true
				update.Policy.SelectedGoal = policy.SelectedGoal
			}
			// Set rank first so status-triggered durable event snapshots include
			// the final column position. A rejected workflow rolls this back too.
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET rank=? WHERE id=? AND project_ident=?", i, t.ID, project); err != nil {
				return err
			}
			if _, err := applyTaskWorkflow(ctx, tx, t, update, actor, s.routerReviews.Load()); err != nil {
				return err
			}
		}
		if crossing && status != "in_progress" {
			return fmt.Errorf("%w: Did this task mutate any files? Move the task with PATCH status and changeset (file paths or null) before reordering; the task has not moved", ErrTaskConflict)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Tasks(ctx, project, []string{status}, false)
}
