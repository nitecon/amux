package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

type TaskListOptions struct {
	Statuses     []string
	IncludeStale bool
	AllGoals     bool
}

type TaskCreateInput struct {
	Title           string                            `json:"title"`
	ParentID        *string                           `json:"parent_task_id,omitempty"`
	RootReason      *string                           `json:"root_reason,omitempty"`
	Specification   *string                           `json:"specification,omitempty"`
	Goal            json.RawMessage                   `json:"goal,omitempty"`
	Labels          []string                          `json:"labels,omitempty"`
	Description     *string                           `json:"description,omitempty"`
	Hostname        *string                           `json:"hostname,omitempty"`
	Reporter        *string                           `json:"reporter,omitempty"`
	Requester       *storage.RegisteredTargetIdentity `json:"requester,omitempty"`
	CanonicalRemote *string                           `json:"canonical_remote,omitempty"`
	Routing         *TaskRouting                      `json:"routing,omitempty"`
}

// CreateTask creates a task as the authenticated human caller and returns the
// validated task acknowledgement.
func (c *Client) CreateTask(ctx context.Context, project string, input TaskCreateInput) (*storage.Task, error) {
	raw, err := c.TaskCreate(ctx, project, input, nil, nil)
	if err != nil {
		return nil, err
	}
	return taskAcknowledgement(raw, project, "")
}

// AddTaskComment adds a human-authored comment and returns its validated
// acknowledgement.
func (c *Client) AddTaskComment(ctx context.Context, project, taskID, content string) (*storage.TaskComment, error) {
	raw, err := c.TaskComment(ctx, project, taskID, content, nil)
	if err != nil {
		return nil, err
	}
	var comment storage.TaskComment
	if json.Unmarshal(raw, &comment) != nil || comment.TaskID != taskID || comment.Author != "user" || comment.AuthorType != "user" || comment.Content != content || comment.Origin != nil {
		return nil, fmt.Errorf("gateway comment acknowledgement did not match the requested task, content, or human provenance")
	}
	id, err := uuid.Parse(comment.ID)
	if err != nil || id == uuid.Nil {
		return nil, fmt.Errorf("gateway comment acknowledgement had an invalid ID")
	}
	return &comment, nil
}

func taskPath(project string) string { return "/v1/projects/" + url.PathEscape(project) + "/tasks" }

func (c *Client) TaskList(ctx context.Context, project string, options TaskListOptions, origin *storage.AgentOrigin, goal *string) ([]storage.TaskSummary, error) {
	query := url.Values{}
	if len(options.Statuses) > 0 {
		query.Set("status", strings.Join(options.Statuses, ","))
	}
	if options.IncludeStale {
		query.Set("include_stale", "true")
	}
	if options.AllGoals {
		query.Set("all_goals", "true")
	}
	if goal != nil {
		query.Set("goal", *goal)
	}
	var result []storage.TaskSummary
	err := c.doTaskGoal(ctx, "GET", taskPath(project)+"?"+query.Encode(), nil, origin, goal, &result)
	return result, err
}

func (c *Client) TaskRead(ctx context.Context, project, id string, comments uint64) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.do(ctx, "GET", taskPath(project)+"/"+url.PathEscape(id)+"?comments="+strconv.FormatUint(comments, 10), nil, nil, &result)
	if err == nil {
		_, err = taskAcknowledgement(result, project, id)
	}
	return result, err
}

func (c *Client) TaskCreate(ctx context.Context, project string, input TaskCreateInput, origin *storage.AgentOrigin, goal *string) (json.RawMessage, error) {
	routed := input.Routing != nil
	if routed {
		if origin != nil {
			return nil, fmt.Errorf("routed task creation requires a human caller")
		}
		if input.Requester != nil {
			return nil, fmt.Errorf("routed task creation does not support a requester subscription")
		}
		if err := c.validateCreateRouting(ctx, project, &input); err != nil {
			return nil, err
		}
		if input.Routing.Mode == TaskRoutingAuto {
			input.Routing = nil
		}
	} else if input.CanonicalRemote != nil {
		return nil, fmt.Errorf("canonical_remote is supported only with routed task creation")
	}
	if input.Requester != nil {
		if err := validateRequesterTarget(input.Requester, origin); err != nil {
			return nil, err
		}
		if err := c.RequireRequesterSubscriptions(ctx); err != nil {
			return nil, err
		}
	}
	if input.Reporter == nil && origin != nil {
		reporter := origin.SessionID
		input.Reporter = &reporter
	}
	var result json.RawMessage
	err := c.doTaskGoal(ctx, "POST", taskPath(project), input, origin, goal, &result)
	if err != nil && routed {
		return result, fmt.Errorf("routed task creation not confirmed; inspect the task before retrying: %w", err)
	}
	if err == nil {
		var task *storage.Task
		task, err = taskAcknowledgement(result, project, "")
		if err == nil {
			id, parseErr := uuid.Parse(task.ID)
			if parseErr != nil || id == uuid.Nil || (input.ParentID == nil && task.Parent != nil) || (input.ParentID != nil && (task.Parent == nil || task.Parent.ID != *input.ParentID || task.Parent.ProjectIdent != projects.NormalizeIdent(project))) {
				err = fmt.Errorf("gateway did not acknowledge the requested task hierarchy")
			}
		}
		if err == nil && input.Requester != nil {
			err = requesterAcknowledgement(result, input.Requester)
		}
		if err == nil && routed && input.Routing != nil {
			err = routedTaskAcknowledgement(task, *input.Routing)
		}
	}
	if err != nil && routed {
		return result, fmt.Errorf("routed task creation not confirmed; inspect the task before retrying: %w", err)
	}
	if err != nil && input.Requester != nil {
		return result, fmt.Errorf("requester creation not confirmed; inspect the task before retrying: %w", err)
	}
	return result, err
}

func (c *Client) TaskUpdate(ctx context.Context, project, id string, input storage.TaskWorkflowUpdate, origin *storage.AgentOrigin, goal *string) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.doTaskGoal(ctx, "PATCH", taskPath(project)+"/"+url.PathEscape(id), input, origin, goal, &result)
	if err == nil {
		var task *storage.Task
		task, err = taskAcknowledgement(result, project, id)
		if err == nil && input.Status != "" && task.Status != input.Status {
			err = fmt.Errorf("gateway did not acknowledge the requested task status")
		}
		if err == nil && input.Status == "in_progress" && origin != nil && (task.OwnerOrigin == nil || *task.OwnerOrigin != *origin) {
			err = fmt.Errorf("gateway did not acknowledge the claiming execution")
		}
		if err == nil && input.Rank != nil && task.Rank != *input.Rank {
			err = fmt.Errorf("gateway did not acknowledge the requested rank")
		}
		if err == nil && input.Status == "" && input.Labels != nil && strings.TrimSpace(string(input.Labels)) != "null" {
			var labels []string
			if json.Unmarshal(input.Labels, &labels) != nil || !slices.Equal(task.Labels, labels) {
				err = fmt.Errorf("gateway did not acknowledge the requested labels")
			}
		}
	}
	return result, err
}

func (c *Client) TaskConfirm(ctx context.Context, project, id string, taskType *string, changeset json.RawMessage, origin *storage.AgentOrigin) (json.RawMessage, error) {
	var result json.RawMessage
	input := struct {
		TaskType  *string         `json:"task_type,omitempty"`
		Changeset json.RawMessage `json:"changeset,omitempty"`
	}{taskType, changeset}
	err := c.do(ctx, "POST", taskPath(project)+"/"+url.PathEscape(id)+"/confirmation", input, origin, &result)
	if err == nil {
		var hint struct {
			TaskID   string `json:"task_id"`
			NextTool string `json:"next_tool"`
		}
		if json.Unmarshal(result, &hint) == nil && hint.TaskID != "" {
			if hint.TaskID != id || hint.NextTool != "task_set_changeset" {
				err = fmt.Errorf("gateway confirmation did not match the task")
			}
		} else {
			_, err = taskAcknowledgement(result, project, id)
		}
	}
	return result, err
}

func (c *Client) TaskComment(ctx context.Context, project, id, content string, origin *storage.AgentOrigin) (json.RawMessage, error) {
	author, kind := "user", "user"
	if origin != nil {
		author, kind = origin.SessionID, "agent"
	}
	input := struct {
		Content string `json:"content"`
		Author  string `json:"author"`
		Kind    string `json:"author_type"`
	}{content, author, kind}
	var result json.RawMessage
	err := c.do(ctx, "POST", taskPath(project)+"/"+url.PathEscape(id)+"/comments", input, origin, &result)
	if err == nil {
		var comment storage.TaskComment
		if json.Unmarshal(result, &comment) != nil || comment.TaskID != id || comment.Author != author || comment.AuthorType != kind || (origin != nil && (comment.Origin == nil || *comment.Origin != *origin)) {
			err = fmt.Errorf("gateway comment did not match the task or author")
		}
	}
	return result, err
}

func taskAcknowledgement(raw json.RawMessage, project, id string) (*storage.Task, error) {
	var task storage.Task
	if json.Unmarshal(raw, &task) != nil {
		return nil, fmt.Errorf("gateway task response did not match the requested scope")
	}
	if err := validateTaskAcknowledgement(&task, project, id); err != nil {
		return nil, err
	}
	return &task, nil
}

func validateTaskAcknowledgement(task *storage.Task, project, id string) error {
	if task == nil || task.ID == "" || (id != "" && task.ID != id) || task.ProjectIdent != projects.NormalizeIdent(project) {
		return fmt.Errorf("gateway task response did not match the requested scope")
	}
	switch task.Status {
	case "todo", "in_progress", "user_review", "done", "cancelled":
		return nil
	}
	return fmt.Errorf("gateway task response has an invalid status")
}
