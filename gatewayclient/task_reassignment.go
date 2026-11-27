package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/taskplatform"
)

// ReassignTask transfers an ordinary active task to one live execution bound
// to the same repository. The gateway mutation is sent once; callers must
// inspect the task after any uncertain response.
func (c *Client) ReassignTask(ctx context.Context, project, id, recipient string, actor storage.AgentOrigin) (*storage.Task, error) {
	project = projects.NormalizeIdent(project)
	if !canonicalDelegationID(id) {
		return nil, fmt.Errorf("task ID must be a canonical non-nil UUID")
	}
	session, err := uuid.Parse(recipient)
	if err != nil || session == uuid.Nil || session.String() != recipient {
		return nil, fmt.Errorf("recipient session must be a canonical non-nil UUID")
	}
	normalized, err := storage.NormalizeOrigin(&actor)
	if err != nil || normalized == nil {
		return nil, fmt.Errorf("reassignment requires a complete native actor")
	}
	actor = *normalized
	current, err := c.TaskDetail(ctx, project, id)
	if err != nil {
		return nil, err
	}
	if current.Task.Status != "todo" && !storage.IsActiveTaskStatus(current.Task.Status) {
		return nil, fmt.Errorf("only active tasks can be reassigned")
	}
	if current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != actor {
		return nil, fmt.Errorf("only the exact owning agent may reassign this task")
	}
	requiredOS, err := taskplatform.RequiredOS(current.Task.Labels)
	if err != nil {
		return nil, err
	}
	directory, err := c.boundTaskTargetDirectory(ctx, project)
	if err != nil {
		return nil, err
	}
	var target *storage.RegisteredTargetIdentity
	selected, err := selectLiveTaskTargets(directory, requiredOS, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	for _, entry := range selected {
		if entry.Target.Origin.SessionID != session.String() {
			continue
		}
		if target != nil {
			return nil, fmt.Errorf("recipient has multiple live compatible executions; refresh targets")
		}
		copy := entry.Target
		target = &copy
	}
	if target == nil {
		return nil, fmt.Errorf("recipient is unavailable, incompatible, or not bound to this project")
	}
	var result json.RawMessage
	path := taskPath(project) + "/" + url.PathEscape(id) + "/reassign"
	if err := c.do(ctx, http.MethodPost, path, struct {
		Target storage.RegisteredTargetIdentity `json:"target"`
	}{*target}, &actor, &result); err != nil {
		return nil, fmt.Errorf("reassignment not confirmed; inspect the task before retrying: %w", err)
	}
	return validateTaskAssignmentReceipt(result, project, id, *target, requiredOS, "reassignment")
}

// AssignTask assigns a task from the authenticated human session to the exact
// selected live target. The server revalidates target generation and OS inside
// the atomic assignment transaction.
func (c *Client) AssignTask(ctx context.Context, project, id string, target storage.RegisteredTargetIdentity) (*storage.Task, error) {
	project = projects.NormalizeIdent(project)
	if !canonicalDelegationID(id) {
		return nil, fmt.Errorf("task ID must be a canonical non-nil UUID")
	}
	origin, err := storage.NormalizeOrigin(&target.Origin)
	if err != nil || origin == nil {
		return nil, fmt.Errorf("assignment requires a complete target origin")
	}
	target.Origin = *origin
	if !canonicalDelegationID(target.Generation) {
		return nil, fmt.Errorf("target generation must be a canonical non-nil UUID")
	}
	detail, err := c.TaskDetail(ctx, project, id)
	if err != nil {
		return nil, err
	}
	if detail.Task.Status != "todo" && !storage.IsActiveTaskStatus(detail.Task.Status) {
		return nil, fmt.Errorf("only active tasks can be assigned")
	}
	requiredOS, err := taskplatform.RequiredOS(detail.Task.Labels)
	if err != nil {
		return nil, err
	}
	directory, err := c.boundTaskTargetDirectory(ctx, project)
	if err != nil {
		return nil, err
	}
	live, err := selectLiveTaskTargets(directory, requiredOS, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	matched := false
	for _, entry := range live {
		if entry.Target == target {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("target is unavailable, incompatible, or not bound to this project")
	}
	var result json.RawMessage
	path := taskPath(project) + "/" + url.PathEscape(id) + "/assign"
	if err := c.do(ctx, http.MethodPost, path, struct {
		Target storage.RegisteredTargetIdentity `json:"target"`
	}{target}, nil, &result); err != nil {
		return nil, fmt.Errorf("assignment not confirmed; inspect the task before retrying: %w", err)
	}
	return validateTaskAssignmentReceipt(result, project, id, target, requiredOS, "assignment")
}

func (c *Client) boundTaskTargetDirectory(ctx context.Context, project string) (storage.TargetDirectory, error) {
	projectsList, err := c.Projects(ctx)
	if err != nil {
		return storage.TargetDirectory{}, err
	}
	var binding *string
	for i := range projectsList {
		if projectsList[i].Ident == project {
			binding = projectsList[i].CanonicalRemote
			break
		}
	}
	if binding == nil {
		return storage.TargetDirectory{}, fmt.Errorf("project repository binding is unavailable")
	}
	directory, err := c.taskTargetDirectory(ctx, project)
	if err != nil {
		return storage.TargetDirectory{}, err
	}
	if directory.CanonicalRemote == nil || *directory.CanonicalRemote != *binding {
		return storage.TargetDirectory{}, fmt.Errorf("task target directory does not match the project repository")
	}
	return directory, nil
}

func validateTaskAssignmentReceipt(raw json.RawMessage, project, id string, target storage.RegisteredTargetIdentity, requiredOS, operation string) (*storage.Task, error) {
	ack, err := taskAcknowledgement(raw, project, id)
	if err != nil || ack.Status != "todo" || ack.OwnerOrigin == nil || *ack.OwnerOrigin != target.Origin {
		return nil, fmt.Errorf("%s receipt did not confirm the requested owner; inspect the task before retrying", operation)
	}
	var routing struct {
		Mode   string                           `json:"mode"`
		Target storage.RegisteredTargetIdentity `json:"target"`
	}
	if json.Unmarshal(ack.Routing, &routing) != nil || routing.Mode != "session" || routing.Target != target {
		return nil, fmt.Errorf("%s receipt did not confirm exact target routing; inspect the task before retrying", operation)
	}
	gotOS, err := taskplatform.RequiredOS(ack.Labels)
	if err != nil || gotOS != requiredOS {
		return nil, fmt.Errorf("%s receipt changed the task OS requirement; inspect the task before retrying", operation)
	}
	return ack, nil
}

func (c *Client) taskTargetDirectory(ctx context.Context, project string) (storage.TargetDirectory, error) {
	var directory storage.TargetDirectory
	if err := c.do(ctx, http.MethodGet, taskTargetsPath(project), nil, nil, &directory); err != nil {
		return directory, err
	}
	return directory, nil
}
