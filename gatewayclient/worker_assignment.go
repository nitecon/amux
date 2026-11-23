package gatewayclient

import (
	"context"
	"net/url"

	"github.com/nitecon/amux/storage"
)

func (c *Client) AdoptWorkerAssignment(ctx context.Context, project string, origin storage.AgentOrigin) (*storage.WorkerAssignment, error) {
	return c.AdoptWorkerAssignmentWithGoal(ctx, project, origin, nil)
}

// AdoptWorkerAssignmentWithGoal preserves the selected queue when adopting a
// committed native assignment. Retries return its existing conversation/prompt.
func (c *Client) AdoptWorkerAssignmentWithGoal(ctx context.Context, project string, origin storage.AgentOrigin, goal *string) (*storage.WorkerAssignment, error) {
	var result *storage.WorkerAssignment
	err := c.doTaskGoal(ctx, "POST", "/v1/projects/"+url.PathEscape(project)+"/worker-assignment", struct{}{}, &origin, goal, &result)
	return result, err
}
