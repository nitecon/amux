package gatewayclient

import (
	"context"
	"errors"
	"net/url"

	"github.com/nitecon/amux/storage"
)

// OwnedActiveTask checks the current task snapshot before a client launches
// a provider. An assignment event alone cannot authorize execution.
func (c *Client) OwnedActiveTask(ctx context.Context, project, id string, owner storage.AgentOrigin) (*storage.Task, error) {
	detail, err := c.TaskDetail(ctx, project, id)
	if err != nil {
		var response *HTTPError
		if errors.As(err, &response) && response.Status == 404 {
			return nil, nil
		}
		return nil, err
	}
	task := &detail.Task
	if task.ID != id || task.OwnerOrigin == nil || *task.OwnerOrigin != owner {
		return nil, nil
	}
	if task.Status != "todo" && !storage.IsActiveTaskStatus(task.Status) {
		return nil, nil
	}
	return task, nil
}

// StartOwnedTask starts work through the legacy task API after verifying the
// exact assignment. The returned task must still belong to the same origin.
func (c *Client) StartOwnedTask(ctx context.Context, project, id string, owner storage.AgentOrigin) (*storage.Task, error) {
	task, err := c.OwnedActiveTask(ctx, project, id, owner)
	if err != nil || task == nil || storage.IsActiveTaskStatus(task.Status) {
		return task, err
	}
	var started storage.Task
	err = c.doTaskGoal(ctx, "PATCH", taskPath(project)+"/"+url.PathEscape(id), struct {
		Status string `json:"status"`
	}{"in_progress"}, &owner, task.Goal, &started)
	if err != nil {
		var response *HTTPError
		if errors.As(err, &response) && (response.Status == 404 || response.Status == 409) {
			return nil, nil
		}
		return nil, err
	}
	if err = validateTaskAcknowledgement(&started, project, id); err != nil {
		return nil, err
	}
	if !storage.IsActiveTaskStatus(started.Status) || started.OwnerOrigin == nil || *started.OwnerOrigin != owner {
		return nil, nil
	}
	return &started, nil
}
