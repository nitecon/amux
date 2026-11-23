package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/nitecon/amux/projects"
)

var (
	ErrSubtaskParentNotFound = errors.New("parent task not found")
	ErrSubtaskTargetNotFound = errors.New("target project not found")
	ErrSubtaskTargetArchived = errors.New("target project is archived")
)

type TaskSubtaskInsert struct {
	ParentProject string
	ParentID      string
	TargetProject *string
	Title         string
	Description   *string
	Specification *string
	Labels        []string
	Reporter      string
}

// CreateTaskSubtask inserts the child, parent link, and creation snapshots in
// one task mutation. The parent is scoped to ParentProject; a child may live
// in a different target project.
func (s *Store) CreateTaskSubtask(ctx context.Context, input TaskSubtaskInsert, origin *AgentOrigin) (*Task, error) {
	input.ParentProject = projects.NormalizeIdent(input.ParentProject)
	if input.ParentProject == "" || input.ParentID == "" {
		return nil, ErrSubtaskParentNotFound
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return nil, fmt.Errorf("%w: title must be non-empty", ErrTaskMetadataInput)
	}
	targetProject := input.ParentProject
	if input.TargetProject != nil {
		targetProject = projects.NormalizeIdent(*input.TargetProject)
	}
	var created *Task
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		parent, err := readTask(ctx, tx, input.ParentProject, input.ParentID)
		if err != nil {
			return err
		}
		if parent == nil {
			return ErrSubtaskParentNotFound
		}
		target, err := getProject(ctx, tx, targetProject)
		if err != nil {
			return err
		}
		if target == nil {
			return ErrSubtaskTargetNotFound
		}
		if target.ArchivedAt != nil {
			return ErrSubtaskTargetArchived
		}
		var goal *string
		if targetProject == parent.ProjectIdent {
			goal = parent.Goal
		}
		created, err = insertTaskRecord(ctx, tx, TaskInsert{
			Project: targetProject, Title: input.Title, Description: input.Description,
			Details: input.Specification, Labels: input.Labels, Reporter: input.Reporter, Goal: goal,
		}, actor, "general")
		if err != nil {
			return err
		}
		if err := attachTaskParent(ctx, tx, parent, created.ID); err != nil {
			return err
		}
		created.Parent = &TaskParent{ID: parent.ID, ProjectIdent: parent.ProjectIdent, Title: parent.Title}
		return nil
	})
	return created, err
}

// TaskSubtasks returns children ordered by creation time and ID after checking
// the parent in the route's project.
func (s *Store) TaskSubtasks(ctx context.Context, project, parentID string) ([]Task, error) {
	project = projects.NormalizeIdent(project)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	parent, err := readTask(ctx, tx, project, parentID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, ErrSubtaskParentNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id,t.project_ident FROM task_subtasks s JOIN tasks t ON t.id=s.child_id WHERE s.parent_id=? ORDER BY t.created_at,t.id`, parentID)
	if err != nil {
		return nil, err
	}
	type taskRef struct{ id, project string }
	refs := make([]taskRef, 0)
	for rows.Next() {
		var ref taskRef
		if err := rows.Scan(&ref.id, &ref.project); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	children := make([]Task, 0, len(refs))
	for _, ref := range refs {
		child, err := readTask(ctx, tx, ref.project, ref.id)
		if err != nil {
			return nil, err
		}
		if child != nil {
			children = append(children, *child)
		}
	}
	return children, nil
}
