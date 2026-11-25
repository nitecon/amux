package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/nitecon/amux/storage"
)

func (c *Client) Projects(ctx context.Context) ([]storage.ProjectOverview, error) {
	var result []storage.ProjectOverview
	err := c.do(ctx, "GET", "/v1/projects", nil, nil, &result)
	return result, err
}

// Checkouts returns project machine origins from the Rust-compatible checkout
// inventory endpoint.
func (c *Client) Checkouts(ctx context.Context) ([]storage.CheckoutOverview, error) {
	var result []storage.CheckoutOverview
	err := c.do(ctx, "GET", "/v1/checkouts", nil, nil, &result)
	return result, err
}

// SyncCheckouts replaces this machine's snapshot, then reads the shared
// project/machine inventory. Origin values are the checkout's observed clone
// source and remain display metadata; they do not bind project identity.
func (c *Client) SyncCheckouts(ctx context.Context, hostname string, checkouts []storage.CheckoutReportItem) ([]storage.CheckoutOverview, error) {
	if strings.TrimSpace(hostname) == "" {
		return nil, fmt.Errorf("checkout report hostname is required")
	}
	report := storage.MachineCheckoutReport{
		Hostname:  strings.TrimSpace(hostname),
		Checkouts: make([]storage.CheckoutReportItem, 0, len(checkouts)),
	}
	report.Checkouts = append(report.Checkouts, checkouts...)
	if err := c.do(ctx, "PUT", "/v1/checkouts", report, nil, nil); err != nil {
		return nil, err
	}
	return c.Checkouts(ctx)
}

func (c *Client) Tasks(ctx context.Context, project string, statuses []string) ([]storage.TaskSummary, error) {
	query := url.Values{"all_goals": {"true"}}
	if len(statuses) > 0 {
		query.Set("status", strings.Join(statuses, ","))
	}
	var result []storage.TaskSummary
	err := c.do(ctx, "GET", "/v1/projects/"+url.PathEscape(project)+"/tasks?"+query.Encode(), nil, nil, &result)
	return result, err
}

// ReorderTasks atomically sets the order of every task in one status column.
func (c *Client) ReorderTasks(ctx context.Context, project, status string, order []string) ([]storage.TaskSummary, error) {
	if status != "todo" && status != "in_progress" && status != "user_review" && status != "done" {
		return nil, fmt.Errorf("invalid task status %q", status)
	}
	query := url.Values{"status": {status}}
	var result []storage.TaskSummary
	err := c.do(ctx, "POST", "/v1/projects/"+url.PathEscape(project)+"/tasks/reorder?"+query.Encode(), map[string][]string{"order": order}, nil, &result)
	if err == nil {
		if len(result) != len(order) {
			return nil, fmt.Errorf("gateway reorder acknowledgement contained %d tasks, want %d", len(result), len(order))
		}
		for i, task := range result {
			if task.ID != order[i] || task.Status != status || task.Rank != int64(i) {
				return nil, fmt.Errorf("gateway reorder acknowledgement did not match the requested order")
			}
		}
	}
	return result, err
}

// TaskEvents returns the gateway's bounded recent activity for a project.
func (c *Client) TaskEvents(ctx context.Context, project string) ([]storage.TaskEvent, error) {
	return c.TaskEventsAfter(ctx, project, nil)
}

func (c *Client) TaskEventsAfter(ctx context.Context, project string, after *int64) ([]storage.TaskEvent, error) {
	query := url.Values{"project": {project}}
	if after != nil {
		query.Set("after_event_id", strconv.FormatInt(*after, 10))
	}
	var events []storage.TaskEvent
	err := c.do(ctx, "GET", "/v1/task-events?"+query.Encode(), nil, nil, &events)
	return events, err
}

func (c *Client) TaskDetail(ctx context.Context, project, id string) (*storage.TaskDetail, error) {
	var result struct {
		storage.Task
		TaskType      string                `json:"task_type"`
		Specification *string               `json:"specification"`
		Comments      []storage.TaskComment `json:"comments"`
		Count         int64                 `json:"comment_count"`
	}
	err := c.do(ctx, "GET", "/v1/projects/"+url.PathEscape(project)+"/tasks/"+url.PathEscape(id), nil, nil, &result)
	if err != nil {
		return nil, err
	}
	if err := validateTaskAcknowledgement(&result.Task, project, id); err != nil {
		return nil, err
	}
	if result.Details == nil {
		result.Details = result.Specification
	}
	result.Task.TaskType = result.TaskType
	return &storage.TaskDetail{Task: result.Task, Comments: result.Comments, CommentCount: result.Count}, nil
}

func (c *Client) TaskSubtasks(ctx context.Context, project, id string) ([]storage.Task, error) {
	var result []storage.Task
	err := c.do(ctx, "GET", taskPath(project)+"/"+url.PathEscape(id)+"/subtasks", nil, nil, &result)
	return result, err
}

func (c *Client) TaskConversations(ctx context.Context, project, taskID, afterID string, limit int) ([]storage.Conversation, error) {
	path := "/v1/projects/" + url.PathEscape(project) + "/tasks/" + url.PathEscape(taskID) + "/conversations"
	return c.listConversations(ctx, path, afterID, limit)
}

func (c *Client) ProjectConversations(ctx context.Context, project, afterID string, limit int) ([]storage.Conversation, error) {
	return c.listConversations(ctx, "/v1/projects/"+url.PathEscape(project)+"/conversations", afterID, limit)
}

func (c *Client) listConversations(ctx context.Context, path, afterID string, limit int) ([]storage.Conversation, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("conversation list limit must be 1-1000")
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if afterID != "" {
		query.Set("after_id", afterID)
	}
	var result []storage.Conversation
	err := c.do(ctx, "GET", path+"?"+query.Encode(), nil, nil, &result)
	return result, err
}

// RegisterRepository registers an explicitly configured project through the
// gateway's project registration API.
func (c *Client) RegisterRepository(ctx context.Context, ident, remote string, recreate bool) error {
	return c.do(ctx, "POST", "/v1/projects", struct {
		Ident    string `json:"ident"`
		Remote   string `json:"repo_url"`
		Recreate bool   `json:"recreate,omitempty"`
	}{ident, remote, recreate}, nil, nil)
}

func (c *Client) DeleteProject(ctx context.Context, ident, confirmation string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(ident),
		map[string]string{"confirmation": confirmation}, nil, nil)
}
