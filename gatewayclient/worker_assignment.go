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
	return c.AdoptWorkerTask(ctx, project, "", origin, goal)
}

func (c *Client) AdoptWorkerTask(ctx context.Context, project, taskID string, origin storage.AgentOrigin, goal *string) (*storage.WorkerAssignment, error) {
	var result *storage.WorkerAssignment
	request := struct {
		TaskID string `json:"task_id,omitempty"`
	}{TaskID: taskID}
	err := c.doTaskGoal(ctx, "POST", "/v1/projects/"+url.PathEscape(project)+"/worker-assignment", request, &origin, goal, &result)
	return result, err
}

// SupportsNativeWorkerAssignment advertises the full native worker workflow,
// including assignment adoption and task conversations.
func (c *Client) SupportsNativeWorkerAssignment(ctx context.Context) (bool, error) {
	var capabilities map[string]int
	err := c.do(ctx, "GET", "/v1/tasks/capabilities", nil, nil, &capabilities)
	return capabilities["native_worker_assignment"] == 1, err
}
