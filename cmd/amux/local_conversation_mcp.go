package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/storage"
)

func localConversationTaskTool(name string) bool {
	switch name {
	case "task_get", "project_message_thread", "task_list", "task_create",
		"task_delegate", "task_update_delegation", "task_set_requester",
		"task_claim", "task_started", "task_comment", "task_release",
		"task_user_review", "task_done", "task_confirm_mutation", "task_set_changeset":
		return true
	}
	return false
}

func localGatewayToolJSON(value any, err error) mcpstdio.Result {
	if err != nil {
		return localGatewayToolResult(nil, localConversationTaskError(err))
	}
	raw, marshalErr := json.Marshal(value)
	return localGatewayToolResult(raw, marshalErr)
}

func localConversationTaskError(err error) error {
	var response *gatewayclient.HTTPError
	if errors.As(err, &response) && response.Status == http.StatusConflict {
		switch response.Code {
		case "task_confirmation_required":
			return fmt.Errorf("Task needs a changeset. If no files were changed, call task_set_changeset with changeset null. If files were changed, call task_confirm_mutation first, then task_set_changeset with every modified, added or deleted path.")
		case "changeset_confirmation_required":
			return fmt.Errorf("This operation needs a changeset. Retry with changeset null if no files changed, or a JSON array of every modified, added or deleted path.")
		}
	}
	return err
}

func decodeLocalMCPArgs(raw json.RawMessage, allowed ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return nil, errors.New("arguments must be one object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("arguments must be one object")
	}
	valid := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		valid[key] = true
	}
	for key := range fields {
		if !valid[key] {
			return nil, fmt.Errorf("unknown argument %q", key)
		}
	}
	return fields, nil
}

func decodeLocalMCPField(fields map[string]json.RawMessage, key string, target any, required bool) error {
	raw, ok := fields[key]
	if !ok {
		if required {
			return fmt.Errorf("%s is required", key)
		}
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%s cannot be null", key)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid %s", key)
	}
	return nil
}

func callLocalConversationTaskTool(ctx context.Context, client *gatewayclient.Client, project string, origin storage.AgentOrigin, selectedGoal *string, name string, args json.RawMessage) mcpstdio.Result {
	allowed := map[string][]string{
		"task_get": {"id"}, "project_message_thread": {"id"}, "task_list": {"status"},
		"task_create":            {"title", "parent", "root_reason", "specification", "notify_results"},
		"task_delegate":          {"title", "target_project", "parent", "specification", "notify_results"},
		"task_update_delegation": {"id", "title", "description", "specification", "details", "labels"},
		"task_set_requester":     {"id", "clear"},
		"task_claim":             {"id"}, "task_started": {"id"}, "task_comment": {"id", "body"},
		"task_release": {"id", "changeset"}, "task_user_review": {"id", "action", "changeset"}, "task_done": {"id", "changeset"},
		"task_confirm_mutation": {"id"}, "task_set_changeset": {"id", "changeset"},
	}
	fields, err := decodeLocalMCPArgs(args, allowed[name]...)
	if err != nil {
		return localGatewayToolResult(nil, err)
	}
	var id string
	if name != "task_list" && name != "task_create" && name != "task_delegate" && name != "project_message_thread" {
		if err := decodeLocalMCPField(fields, "id", &id, true); err != nil {
			return localGatewayToolResult(nil, err)
		}
		id, err = canonicalTaskID(id)
		if err != nil {
			return localGatewayToolResult(nil, err)
		}
	}
	switch name {
	case "task_get":
		raw, err := client.TaskRead(ctx, project, id, 50)
		return localGatewayToolResult(raw, localConversationTaskError(err))
	case "project_message_thread":
		var messageID int64
		if err := decodeLocalMCPField(fields, "id", &messageID, true); err != nil || messageID <= 0 {
			return localGatewayToolResult(nil, errors.New("id must be a positive integer"))
		}
		thread, err := client.GetProjectMessageThread(ctx, project, messageID)
		return localGatewayToolJSON(thread, err)
	case "task_list":
		statuses := []string{"todo", "in_progress"}
		if _, ok := fields["status"]; ok {
			var status string
			if err := decodeLocalMCPField(fields, "status", &status, true); err != nil {
				return localGatewayToolResult(nil, errors.New("invalid status"))
			}
			statuses = []string{}
			for _, part := range strings.Split(status, ",") {
				if part = strings.TrimSpace(part); part != "" {
					statuses = append(statuses, part)
				}
			}
		}
		for _, status := range statuses {
			if status != "todo" && status != "in_progress" && status != "user_review" && status != "done" && status != "cancelled" {
				return localGatewayToolResult(nil, errors.New("invalid task status"))
			}
		}
		tasks, err := client.TaskList(ctx, project, gatewayclient.TaskListOptions{Statuses: statuses}, &origin, selectedGoal)
		if err == nil && tasks == nil {
			tasks = []storage.TaskSummary{}
		}
		return localGatewayToolJSON(tasks, err)
	case "task_create":
		var input gatewayclient.TaskCreateInput
		if err := decodeLocalMCPField(fields, "title", &input.Title, true); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if err := decodeLocalMCPField(fields, "parent", &input.ParentID, false); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if err := decodeLocalMCPField(fields, "root_reason", &input.RootReason, false); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if err := decodeLocalMCPField(fields, "specification", &input.Specification, false); err != nil {
			return localGatewayToolResult(nil, err)
		}
		var notifyResults bool
		if err := decodeLocalMCPField(fields, "notify_results", &notifyResults, false); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if input.ParentID == nil && input.RootReason == nil {
			return localGatewayToolResult(nil, errors.New("agent task creation requires parent or root_reason with an explanatory specification"))
		}
		if input.RootReason != nil && (input.ParentID != nil || input.Specification == nil || strings.TrimSpace(*input.Specification) == "") {
			return localGatewayToolResult(nil, errors.New("root_reason requires specification and cannot accompany parent"))
		}
		if input.ParentID != nil {
			canonical, err := canonicalTaskID(*input.ParentID)
			if err != nil {
				return localGatewayToolResult(nil, err)
			}
			input.ParentID = &canonical
		}
		if notifyResults {
			input.Requester, err = client.RequesterTarget(ctx, project, origin)
			if err != nil {
				return localGatewayToolResult(nil, err)
			}
		}
		raw, err := client.TaskCreate(ctx, project, input, &origin, selectedGoal)
		return localGatewayToolResult(raw, localConversationTaskError(err))
	case "task_delegate":
		var input gatewayclient.TaskDelegateInput
		if err := decodeLocalMCPField(fields, "title", &input.Title, true); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if err := decodeLocalMCPField(fields, "target_project", &input.TargetProject, true); err != nil {
			return localGatewayToolResult(nil, err)
		}
		var parent string
		if err := decodeLocalMCPField(fields, "parent", &parent, true); err != nil {
			return localGatewayToolResult(nil, errors.New("parent task UUID is required in a local conversation"))
		}
		canonical, err := canonicalTaskID(parent)
		if err != nil {
			return localGatewayToolResult(nil, err)
		}
		input.ParentID = &canonical
		if err := decodeLocalMCPField(fields, "specification", &input.Specification, false); err != nil {
			return localGatewayToolResult(nil, err)
		}
		var notifyResults bool
		if err := decodeLocalMCPField(fields, "notify_results", &notifyResults, false); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if err := client.RequireTaskHierarchy(ctx); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if notifyResults {
			input.Requester, err = client.RequesterTarget(ctx, project, origin)
			if err != nil {
				return localGatewayToolResult(nil, err)
			}
		}
		result, err := client.DelegateTask(ctx, project, input, &origin, selectedGoal)
		return localGatewayToolJSON(result, err)
	case "task_update_delegation":
		var update storage.TaskDelegationUpdate
		rawFields, err := json.Marshal(fields)
		if err != nil || json.Unmarshal(rawFields, &update) != nil {
			return localGatewayToolResult(nil, errors.New("invalid delegation metadata"))
		}
		if update.Title == nil && update.Description == nil && update.Details == nil && update.Specification == nil && update.Labels == nil {
			return localGatewayToolResult(nil, errors.New("provide at least one metadata field"))
		}
		result, err := client.UpdateTaskDelegation(ctx, project, id, update, &origin)
		return localGatewayToolJSON(result, err)
	case "task_set_requester":
		clear := false
		if err := decodeLocalMCPField(fields, "clear", &clear, false); err != nil {
			return localGatewayToolResult(nil, err)
		}
		var requester *storage.RegisteredTargetIdentity
		if !clear {
			requester, err = client.RequesterTarget(ctx, project, origin)
			if err != nil {
				return localGatewayToolResult(nil, err)
			}
		}
		result, err := client.SetTaskRequester(ctx, project, id, requester, &origin)
		return localGatewayToolJSON(result, err)
	case "task_claim":
		raw, err := client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{Status: "in_progress"}, &origin, selectedGoal)
		return localGatewayToolResult(raw, localConversationTaskError(err))
	case "task_started":
		task, err := client.OwnedActiveTask(ctx, project, id, origin)
		if err != nil {
			return localGatewayToolResult(nil, localConversationTaskError(err))
		}
		if task == nil || task.Status != "in_progress" {
			return localGatewayToolResult(nil, storage.ErrTaskOwnership)
		}
		return localGatewayToolJSON(task, nil)
	case "task_comment":
		var body string
		if err := decodeLocalMCPField(fields, "body", &body, true); err != nil {
			return localGatewayToolResult(nil, err)
		}
		if strings.TrimSpace(body) == "" || len(body) > 65536 {
			return localGatewayToolResult(nil, errors.New("body must be non-empty and at most 65536 bytes"))
		}
		raw, err := client.TaskComment(ctx, project, id, body, &origin)
		return localGatewayToolResult(raw, localConversationTaskError(err))
	case "task_release", "task_user_review", "task_done":
		status := map[string]string{"task_release": "todo", "task_user_review": "user_review", "task_done": "done"}[name]
		update := storage.TaskWorkflowUpdate{Status: status}
		if name == "task_user_review" {
			var action string
			if err := decodeLocalMCPField(fields, "action", &action, true); err != nil {
				return localGatewayToolResult(nil, err)
			}
			update.ReviewAction = &action
		}
		if rawChanges, ok := fields["changeset"]; ok {
			update.Changeset = append(json.RawMessage(nil), rawChanges...)
		}
		raw, err := client.TaskUpdate(ctx, project, id, update, &origin, selectedGoal)
		return localGatewayToolResult(raw, localConversationTaskError(err))
	case "task_confirm_mutation":
		taskType := "software"
		raw, err := client.TaskConfirm(ctx, project, id, &taskType, nil, &origin)
		return localGatewayToolResult(raw, localConversationTaskError(err))
	case "task_set_changeset":
		rawChanges, ok := fields["changeset"]
		if !ok {
			return localGatewayToolResult(nil, errors.New("changeset is required (use null for no mutations)"))
		}
		raw, err := client.TaskConfirm(ctx, project, id, nil, append(json.RawMessage(nil), rawChanges...), &origin)
		return localGatewayToolResult(raw, localConversationTaskError(err))
	default:
		return localGatewayToolResult(nil, errors.New("unsupported task tool"))
	}
}
