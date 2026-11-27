package gatewayclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/nitecon/amux/storage"
)

// CloseTask closes a task as the authenticated human caller. It deliberately
// sends no changeset; agent completion remains subject to guided confirmation.
func (c *Client) CloseTask(ctx context.Context, project, id string) (*storage.Task, error) {
	return c.updateDesktopTask(ctx, project, id, storage.TaskWorkflowUpdate{Status: "done"}, func(task *storage.Task) error {
		if task.DoneAt == nil {
			return fmt.Errorf("gateway did not acknowledge task closure")
		}
		return nil
	})
}

// ParkTask asks the user to make the supplied decision and clears task
// ownership through the human workflow endpoint.
func (c *Client) ParkTask(ctx context.Context, project, id, reviewAction string) (*storage.Task, error) {
	reviewAction = strings.TrimSpace(reviewAction)
	if len(reviewAction) == 0 || len(reviewAction) > 4096 {
		return nil, fmt.Errorf("review action must be 1 to 4096 bytes")
	}
	current, err := c.readDesktopTask(ctx, project, id)
	if err != nil {
		return nil, err
	}
	if current.Status != "todo" && !storage.IsActiveTaskStatus(current.Status) {
		return nil, fmt.Errorf("only todo or in-progress tasks can be parked")
	}
	return c.updateDesktopTask(ctx, project, id, storage.TaskWorkflowUpdate{
		Status:         "user_review",
		ReviewAction:   &reviewAction,
		ExpectedStatus: &current.Status,
	}, func(task *storage.Task) error {
		if task.ReviewAction == nil || *task.ReviewAction != reviewAction || task.OwnerAgentID != nil || task.OwnerOrigin != nil || task.StartedAt != nil {
			return fmt.Errorf("gateway did not acknowledge the requested user review")
		}
		return nil
	})
}

// ResumeTask returns a task in user review to the queue as the authenticated
// human caller. The pre-read preserves desktop behavior; expected_status makes
// the update conditional on that observed state.
func (c *Client) ResumeTask(ctx context.Context, project, id string) (*storage.Task, error) {
	current, err := c.readDesktopTask(ctx, project, id)
	if err != nil {
		return nil, err
	}
	if current.Status != "user_review" {
		return nil, fmt.Errorf("only a task in user review can be resumed")
	}
	expectedUserReview := "user_review"
	return c.updateDesktopTask(ctx, project, id, storage.TaskWorkflowUpdate{Status: "todo", ExpectedStatus: &expectedUserReview}, func(task *storage.Task) error {
		if task.ReviewAction != nil || task.OwnerAgentID != nil || task.OwnerOrigin != nil || task.StartedAt != nil || task.DoneAt != nil {
			return fmt.Errorf("gateway did not acknowledge the task return to the queue")
		}
		return nil
	})
}

func (c *Client) readDesktopTask(ctx context.Context, project, id string) (*storage.Task, error) {
	raw, err := c.TaskRead(ctx, project, id, 0)
	if err != nil {
		return nil, err
	}
	return taskAcknowledgement(raw, project, id)
}

func (c *Client) updateDesktopTask(ctx context.Context, project, id string, update storage.TaskWorkflowUpdate, validate func(*storage.Task) error) (*storage.Task, error) {
	raw, err := c.TaskUpdate(ctx, project, id, update, nil, nil)
	if err != nil {
		return nil, err
	}
	task, err := taskAcknowledgement(raw, project, id)
	if err != nil {
		return nil, err
	}
	if task.Status != update.Status {
		return nil, fmt.Errorf("gateway did not acknowledge the requested task status")
	}
	if err := validate(task); err != nil {
		return nil, err
	}
	return task, nil
}
