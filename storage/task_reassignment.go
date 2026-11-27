package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/taskplatform"
)

func ensureTaskNotManagedDispatch(ctx context.Context, q queryer, taskID string) error {
	var table bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='task_dispatches')").Scan(&table); err != nil {
		return err
	}
	if table {
		var managed bool
		if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM task_dispatches WHERE task_id=?)", taskID).Scan(&managed); err != nil {
			return err
		}
		if managed {
			return fmt.Errorf("%w: dispatch tasks use the dispatch handoff API", ErrTaskConflict)
		}
	}
	return nil
}

// resumeTaskOnUserComment queues the reply and preserves the last worker's
// stable session when a compatible live registration still exists.
func resumeTaskOnUserComment(ctx context.Context, tx *sql.Tx, task *Task) error {
	now := time.Now()
	previous := task.CompletionOwner
	if previous == nil {
		previous = task.OwnerOrigin
	}
	var preferred *RegisteredTargetIdentity
	if previous != nil {
		directory, err := readTargetDirectory(ctx, tx, task.ProjectIdent, now)
		if err != nil {
			return err
		}
		osName, err := taskplatform.RequiredOS(task.Labels)
		if err != nil {
			return err
		}
		for _, entry := range directory.Targets {
			origin := entry.Target.Origin
			if origin.SessionID != previous.SessionID || origin.Provider != previous.Provider || (osName != "" && osName != origin.OS) {
				continue
			}
			if preferred != nil {
				preferred = nil
				break
			}
			preferred = &entry.Target
		}
	}
	queued, err := applyTaskWorkflow(ctx, tx, task, TaskWorkflowUpdate{Status: "todo", Policy: TaskTransitionPolicy{HumanRequest: true}}, nil, false)
	if err != nil || preferred == nil {
		return err
	}
	if err := ensureReviewer(ctx, tx, queued, &preferred.Origin); err != nil {
		if errors.Is(err, ErrTaskConflict) || errors.Is(err, ErrTaskOwnership) {
			return nil
		}
		return err
	}
	owner, err := json.Marshal(preferred.Origin)
	if err != nil {
		return err
	}
	route, err := json.Marshal(TaskRouting{Mode: "session", Target: preferred})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET owner_agent_id=?,owner_origin_json=?,routing_json=? WHERE id=? AND project_ident=?`,
		preferred.Origin.SessionID, string(owner), string(route), queued.ID, queued.ProjectIdent)
	return err
}

// ReassignTask atomically transfers an ordinary active task from its exact
// current owner to a fresh, compatible registered execution.
func (s *Store) ReassignTask(ctx context.Context, project, id string, actor AgentOrigin, target RegisteredTargetIdentity, now time.Time) (*Task, error) {
	return s.assignTask(ctx, project, id, &actor, target, now, false)
}

// AssignTaskFromUser assigns an ordinary active task from a human session.
func (s *Store) AssignTaskFromUser(ctx context.Context, project, id string, target RegisteredTargetIdentity, now time.Time) (*Task, error) {
	return s.assignTask(ctx, project, id, nil, target, now, true)
}

func (s *Store) assignTask(ctx context.Context, project, id string, actor *AgentOrigin, target RegisteredTargetIdentity, now time.Time, human bool) (*Task, error) {
	project = projects.NormalizeIdent(project)
	origin, err := NormalizeOrigin(&target.Origin)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid reassignment target origin", ErrTaskMetadataInput)
	}
	target.Origin = *origin
	if !canonicalUUID(target.Generation) || target.Generation == "00000000-0000-0000-0000-000000000000" {
		return nil, fmt.Errorf("%w: reassignment target generation must be a canonical UUID", ErrTaskMetadataInput)
	}

	var result *Task
	err = s.taskMutation(ctx, actor, func(tx *sql.Tx, currentActor *AgentOrigin) error {
		task, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if task == nil {
			return ErrTaskNotFound
		}
		if err := ensureTaskNotManagedDispatch(ctx, tx, task.ID); err != nil {
			return err
		}
		if task.Status != "todo" && !IsActiveTaskStatus(task.Status) {
			return fmt.Errorf("%w: only active tasks can be reassigned", ErrTaskConflict)
		}
		if !human && (currentActor == nil || task.OwnerOrigin == nil || *task.OwnerOrigin != *currentActor) {
			return ErrTaskOwnership
		}
		requiredOS, err := taskplatform.RequiredOS(task.Labels)
		if err != nil {
			return fmt.Errorf("%w: invalid task OS labels: %v", ErrTaskConflict, err)
		}
		if requiredOS != "" && requiredOS != target.Origin.OS {
			return fmt.Errorf("%w: target agent OS does not match task labels", ErrTaskConflict)
		}
		if _, err := validateRegisteredTaskTarget(ctx, tx, task.ProjectIdent, &target, nil, now); err != nil {
			return err
		}
		if err := ensureReviewer(ctx, tx, task, &target.Origin); err != nil {
			return err
		}
		ownerJSON, err := json.Marshal(target.Origin)
		if err != nil {
			return err
		}
		routingJSON, err := json.Marshal(struct {
			Mode   string                   `json:"mode"`
			Target RegisteredTargetIdentity `json:"target"`
		}{Mode: "session", Target: target})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status='todo',owner_agent_id=?,owner_origin_json=?,
			routing_json=?,started_at=NULL,done_at=NULL,updated_at=? WHERE id=? AND project_ident=?`,
			target.Origin.SessionID, string(ownerJSON), string(routingJSON), now.UnixMilli(), id, task.ProjectIdent); err != nil {
			return err
		}
		previous := "unclaimed"
		if task.OwnerOrigin != nil {
			encoded, err := json.Marshal(task.OwnerOrigin)
			if err != nil {
				return err
			}
			previous = string(encoded)
		} else if task.OwnerAgentID != nil {
			previous = *task.OwnerAgentID
		}
		comment := fmt.Sprintf("Task reassigned to agent session %s. Instance at handoff: %s. The worker may start under a new execution instance. Previous assignment: %s.", target.Origin.SessionID, target.Origin.InstanceID, previous)
		commenter, role := "user", "user"
		if currentActor != nil {
			commenter, role = currentActor.SessionID, "agent"
		}
		if _, err := insertTaskComment(ctx, tx, id, commenter, role, comment, nil, currentActor); err != nil {
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
