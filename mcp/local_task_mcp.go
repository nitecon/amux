package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func localGatewayTaskTool(name string) bool {
	switch name {
	case "task_get", "task_comment", "task_ask", "task_started", "task_done", "task_release", "task_user_review", "task_confirm_mutation", "task_set_changeset":
		return true
	}
	return false
}

func verifyLocalGatewayTaskWrite(ctx context.Context, client *gatewayclient.Client, project, assigned string, origin agent.Origin, args json.RawMessage, field string) error {
	var fields map[string]json.RawMessage
	var id string
	if json.Unmarshal(args, &fields) != nil || json.Unmarshal(fields[field], &id) != nil || id != assigned {
		return fmt.Errorf("task write must target the assigned task")
	}
	task, err := client.OwnedActiveTask(ctx, project, assigned, origin)
	if err != nil {
		return err
	}
	if task == nil || !storage.IsActiveTaskStatus(task.Status) {
		return storage.ErrTaskOwnership
	}
	return nil
}

func localGatewayToolResult(raw json.RawMessage, err error) storage.ToolResult {
	text := string(raw)
	if err != nil {
		text = err.Error()
	}
	return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: text}}, IsError: err != nil}
}

func callLocalGatewayTaskTool(ctx context.Context, client *gatewayclient.Client, project, assigned string, origin agent.Origin, goal *string, name string, args json.RawMessage) storage.ToolResult {
	var input struct {
		ID        string          `json:"id"`
		Body      string          `json:"body"`
		Action    string          `json:"action"`
		Question  string          `json:"question"`
		Options   []string        `json:"options"`
		Changeset json.RawMessage `json:"changeset"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return localGatewayToolResult(nil, err)
	}
	id, err := storage.CanonicalTaskID(input.ID)
	if err != nil {
		return localGatewayToolResult(nil, err)
	}
	if name == "task_get" {
		return localGatewayToolResult(client.TaskRead(ctx, project, id, 20))
	}
	if err := verifyLocalGatewayTaskWrite(ctx, client, project, assigned, origin, args, "id"); err != nil {
		return localGatewayToolResult(nil, err)
	}
	switch name {
	case "task_comment":
		return localGatewayToolResult(client.TaskComment(ctx, project, id, input.Body, &origin))
	case "task_ask":
		return localGatewayToolResult(client.AskTaskQuestion(ctx, project, id, input.Question, input.Options, &origin))
	case "task_user_review":
		return localGatewayToolResult(client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{Status: "user_review", ReviewAction: &input.Action, Changeset: input.Changeset}, &origin, goal))
	case "task_set_changeset":
		return localGatewayToolResult(client.TaskConfirm(ctx, project, id, nil, input.Changeset, &origin))
	}
	switch name {
	case "task_started":
		return localGatewayToolResult(client.ReportTaskStarted(ctx, project, id, &origin))
	case "task_done", "task_release":
		if _, err := storage.ValidateChangeset(input.Changeset); input.Changeset != nil && err != nil {
			return localGatewayToolResult(nil, fmt.Errorf("changeset is invalid"))
		}
		if name == "task_release" {
			return localGatewayToolResult(client.ReleaseTask(ctx, project, id, &origin, goal, input.Changeset))
		}
		return localGatewayToolResult(client.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{Status: "done", Changeset: input.Changeset}, &origin, goal))
	case "task_confirm_mutation":
		taskType := "software"
		return localGatewayToolResult(client.TaskConfirm(ctx, project, id, &taskType, nil, &origin))
	default:
		return localGatewayToolResult(nil, fmt.Errorf("unknown task tool: %s", name))
	}
}
