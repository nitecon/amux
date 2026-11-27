package amuxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func localConversationTaskTool(name string) bool {
	for _, tool := range server.LocalConversationTaskTools() {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func localConversationToolResult(raw json.RawMessage, err error) mcpstdio.Result {
	if err != nil {
		err = errors.New(strings.TrimPrefix(err.Error(), "invalid task tool arguments: "))
	}
	return localGatewayToolResult(raw, err)
}

func localConversationToolJSON(value any, err error) mcpstdio.Result {
	if err != nil {
		return localConversationToolResult(nil, err)
	}
	raw, marshalErr := json.Marshal(value)
	return localConversationToolResult(raw, marshalErr)
}

func callLocalConversationTaskTool(ctx context.Context, client *gatewayclient.Client, project string, origin storage.AgentOrigin, selectedGoal *string, name string, args json.RawMessage) mcpstdio.Result {
	switch name {
	case "task_get":
		id, err := server.ParseTaskIDArg(args)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		id, err = canonicalTaskID(id)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		raw, err := client.TaskRead(ctx, project, id, 50)
		return localConversationToolResult(raw, err)
	case "project_message_thread":
		messageID, err := server.ParseProjectMessageThreadArgs(args)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		thread, err := client.GetProjectMessageThread(ctx, project, messageID)
		return localConversationToolJSON(thread, err)
	case "task_list":
		statuses, err := server.ParseTaskListArgs(args)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		tasks, err := client.TaskList(ctx, project, gatewayclient.TaskListOptions{Statuses: statuses}, &origin, selectedGoal)
		if err == nil && tasks == nil {
			tasks = []storage.TaskSummary{}
		}
		return localConversationToolJSON(tasks, err)
	case "task_create":
		parsed, err := server.ParseTaskCreateArgs(args, false)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		input := gatewayclient.TaskCreateInput{
			Title:         parsed.Title,
			ParentID:      parsed.ParentID,
			RootReason:    parsed.RootReason,
			Specification: parsed.Specification,
		}
		if input.ParentID != nil {
			canonical, err := canonicalTaskID(*input.ParentID)
			if err != nil {
				return localConversationToolResult(nil, err)
			}
			input.ParentID = &canonical
		}
		raw, err := client.TaskCreate(ctx, project, input, &origin, selectedGoal)
		return localConversationToolResult(raw, err)
	case "task_delegate":
		parsed, err := server.ParseTaskDelegateArgs(args, true, false)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		canonical, err := canonicalTaskID(*parsed.ParentID)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		if err := client.RequireTaskHierarchy(ctx); err != nil {
			return localConversationToolResult(nil, err)
		}
		result, err := client.DelegateTask(ctx, project, gatewayclient.TaskDelegateInput{
			Title:         parsed.Title,
			TargetProject: parsed.TargetProject,
			ParentID:      &canonical,
			Specification: parsed.Specification,
		}, &origin, selectedGoal)
		return localConversationToolJSON(result, err)
	case "task_comment":
		id, body, err := server.ParseTaskCommentArgs(args)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		id, err = canonicalTaskID(id)
		if err != nil {
			return localConversationToolResult(nil, err)
		}
		raw, err := client.TaskComment(ctx, project, id, body, &origin)
		return localConversationToolResult(raw, err)
	default:
		return localConversationToolResult(nil, errors.New("unsupported task tool"))
	}
}
