package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
)

// SetTaskRequester changes the durable result subscriber without changing task
// status or assignment. Native callers must own the task or be its requester.
func (s *Store) SetTaskRequester(ctx context.Context, project, id string, target *RegisteredTargetIdentity, origin *agent.Origin) (*Task, error) {
	if target != nil {
		normalized, err := agent.NormalizeOrigin(&target.Origin)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid requester origin", ErrTaskDelegation)
		}
		copy := *target
		copy.Origin = *normalized
		target = &copy
	}
	project = projectid.NormalizeIdent(project)
	var result *Task
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *agent.Origin) error {
		task, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if task == nil {
			return ErrTaskNotFound
		}
		if actor != nil {
			isOwner := task.OwnerOrigin != nil && *task.OwnerOrigin == *actor
			isRequester := false
			if len(task.Requester) > 0 && !isJSONNull(task.Requester) {
				var previous RegisteredTargetIdentity
				if err := json.Unmarshal(task.Requester, &previous); err != nil {
					return fmt.Errorf("invalid stored task requester: %w", err)
				}
				isRequester = previous.Origin == *actor
			}
			if !isOwner && !isRequester {
				return ErrTaskOwnership
			}
		}
		if err := validateTaskRequester(ctx, tx, task.ProjectIdent, task.Labels, target, actor); err != nil {
			return err
		}
		var requesterJSON any
		if target != nil {
			encoded, err := json.Marshal(target)
			if err != nil {
				return err
			}
			requesterJSON = string(encoded)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET requester_json=?,updated_at=? WHERE id=? AND project_ident=?", requesterJSON, time.Now().UnixMilli(), id, task.ProjectIdent); err != nil {
			return err
		}
		result, err = readTask(ctx, tx, task.ProjectIdent, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
