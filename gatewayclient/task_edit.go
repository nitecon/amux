package gatewayclient

import (
	"context"
	"net/url"

	"github.com/nitecon/amux/storage"
)

// UpdateTask submits one human metadata edit. Worker-origin task transitions
// use their separately fenced command/tool routes. A failed write is never
// retried automatically: the caller retains its draft and decides what to do.
func (c *Client) UpdateTask(ctx context.Context, project, id string, update storage.TaskMetadataUpdate) (*storage.Task, error) {
	if err := storage.ValidateTaskMetadata(update); err != nil {
		return nil, err
	}
	var result storage.Task
	if err := c.do(ctx, "PATCH", "/v1/projects/"+url.PathEscape(project)+"/tasks/"+url.PathEscape(id), update, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
