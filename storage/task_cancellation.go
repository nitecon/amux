package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

const taskCancellationReasonBytes = 16384

var ErrTaskCancellationInput = errors.New("invalid task cancellation input")

type Cancellation struct {
	TaskID                string       `json:"task_id"`
	ProjectIdent          string       `json:"project_ident"`
	Reason                string       `json:"reason"`
	PriorStatus           string       `json:"prior_status"`
	PriorOwnerAgentID     *string      `json:"prior_owner_agent_id"`
	PriorOwnerOrigin      *AgentOrigin `json:"prior_owner_origin"`
	PriorStartedAt        *int64       `json:"prior_started_at"`
	ReporterOrigin        AgentOrigin  `json:"reporter_origin"`
	CancelledAt           int64        `json:"cancelled_at"`
	ExecutorStopConfirmed bool         `json:"executor_stop_confirmed"`
}

type CancelResponse struct {
	Task         *Task         `json:"task"`
	Cancellation *Cancellation `json:"cancellation"`
}

func migrateTaskCancellations(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS task_cancellations (
		task_id TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
		project_ident TEXT NOT NULL,
		reason TEXT NOT NULL,
		prior_status TEXT NOT NULL,
		prior_owner_agent_id TEXT,
		prior_owner_origin_json TEXT,
		prior_started_at INTEGER,
		reporter_origin_json TEXT NOT NULL,
		cancelled_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(14)")
	return err
}

func ValidateTaskCancellationReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if len(reason) == 0 || len(reason) > taskCancellationReasonBytes {
		return "", fmt.Errorf("%w: reason must be 1..=%d bytes", ErrTaskCancellationInput, taskCancellationReasonBytes)
	}
	return reason, nil
}

func (s *Store) CancelTask(ctx context.Context, project, id string, reason string, changeset json.RawMessage, origin *AgentOrigin) (*CancelResponse, error) {
	if origin == nil {
		return nil, ErrTaskOwnership
	}
	actor, err := NormalizeOrigin(origin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTaskCancellationInput, err)
	}
	reason, err = ValidateTaskCancellationReason(reason)
	if err != nil {
		return nil, err
	}
	var paths []string
	if changeset != nil {
		paths, err = ValidateChangeset(changeset)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTaskCancellationInput, err)
		}
	}

	var response *CancelResponse
	err = s.taskMutation(ctx, actor, func(tx *sql.Tx, actor *AgentOrigin) error {
		task, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if task == nil {
			return ErrTaskNotFound
		}
		if task.Status == "cancelled" {
			existing, err := taskCancellation(ctx, tx, project, id)
			if err != nil {
				return err
			}
			if existing == nil {
				return fmt.Errorf("%w: cancelled task has no cancellation record", ErrTaskConflict)
			}
			if existing.ReporterOrigin == *actor && existing.Reason == reason {
				response = &CancelResponse{Task: task, Cancellation: existing}
				return nil
			}
			return fmt.Errorf("%w: task is already cancelled", ErrTaskConflict)
		}
		if task.Status == "user_review" {
			return fmt.Errorf("%w: tasks in user review require a user decision", ErrTaskConflict)
		}
		if task.Status == "done" {
			return fmt.Errorf("%w: completed tasks cannot be cancelled", ErrTaskConflict)
		}
		if err := ordinaryWorkflowTask(ctx, tx, task); err != nil {
			return err
		}
		owned := task.OwnerAgentID != nil || task.Status == "in_progress"
		if task.OwnerOrigin != nil {
			if task.OwnerOrigin.SessionID != actor.SessionID || task.OwnerOrigin.InstanceID != actor.InstanceID {
				return ErrTaskOwnership
			}
		} else if owned {
			return ErrTaskOwnership
		}
		if changeset == nil {
			return ErrTaskChangeset
		}

		files := append([]string(nil), task.Changeset...)
		for _, path := range paths {
			found := false
			for _, prior := range files {
				if prior == path {
					found = true
					break
				}
			}
			if !found {
				files = append(files, path)
			}
		}
		var merged any
		if len(files) > 0 {
			encoded, err := json.Marshal(files)
			if err != nil {
				return err
			}
			merged = string(encoded)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET changeset_json=? WHERE id=? AND project_ident=?", merged, id, task.ProjectIdent); err != nil {
			return err
		}

		now := time.Now().UnixMilli()
		ownerOrigin, err := json.Marshal(task.OwnerOrigin)
		if err != nil {
			return err
		}
		var ownerOriginJSON any
		if task.OwnerOrigin != nil {
			ownerOriginJSON = string(ownerOrigin)
		}
		reporterOrigin, err := json.Marshal(actor)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_cancellations(task_id,project_ident,reason,prior_status,
			prior_owner_agent_id,prior_owner_origin_json,prior_started_at,reporter_origin_json,cancelled_at)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(task_id) DO UPDATE SET
				project_ident=excluded.project_ident,
				reason=excluded.reason,
				prior_status=excluded.prior_status,
				prior_owner_agent_id=excluded.prior_owner_agent_id,
				prior_owner_origin_json=excluded.prior_owner_origin_json,
				prior_started_at=excluded.prior_started_at,
				reporter_origin_json=excluded.reporter_origin_json,
				cancelled_at=excluded.cancelled_at`, id, task.ProjectIdent, reason, task.Status, task.OwnerAgentID, ownerOriginJSON, task.StartedAt, string(reporterOrigin), now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status='cancelled',updated_at=? WHERE id=? AND project_ident=?", now, id, task.ProjectIdent); err != nil {
			return err
		}
		task, err = readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		cancellation, err := taskCancellation(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if cancellation == nil {
			return fmt.Errorf("task cancellation record missing after insert")
		}
		response = &CancelResponse{Task: task, Cancellation: cancellation}
		return nil
	})
	return response, err
}

func (s *Store) TaskCancellation(ctx context.Context, project, id string) (*Cancellation, error) {
	return taskCancellation(ctx, s.db, project, id)
}

func taskCancellation(ctx context.Context, q queryer, project, id string) (*Cancellation, error) {
	var cancellation Cancellation
	var ownerOrigin, reporterOrigin *string
	err := q.QueryRowContext(ctx, `SELECT task_id,project_ident,reason,prior_status,prior_owner_agent_id,
		prior_owner_origin_json,prior_started_at,reporter_origin_json,cancelled_at
		FROM task_cancellations WHERE task_id=? AND project_ident=?`, id, projects.NormalizeIdent(project)).Scan(
		&cancellation.TaskID, &cancellation.ProjectIdent, &cancellation.Reason, &cancellation.PriorStatus,
		&cancellation.PriorOwnerAgentID, &ownerOrigin, &cancellation.PriorStartedAt, &reporterOrigin, &cancellation.CancelledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ownerOrigin != nil {
		var origin AgentOrigin
		if err := json.Unmarshal([]byte(*ownerOrigin), &origin); err != nil {
			return nil, err
		}
		cancellation.PriorOwnerOrigin = &origin
	}
	if reporterOrigin == nil {
		return nil, fmt.Errorf("task cancellation has no reporter origin")
	}
	if err := json.Unmarshal([]byte(*reporterOrigin), &cancellation.ReporterOrigin); err != nil {
		return nil, err
	}
	// Executor stop is never observable by the gateway.
	cancellation.ExecutorStopConfirmed = false
	return &cancellation, nil
}
