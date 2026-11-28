package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/storage"
)

func (c *Client) CallTaskTool(ctx context.Context, project, conversationID string, origin agent.Origin, name string, arguments json.RawMessage) (storage.ToolResult, error) {
	return c.CallTaskToolWithGoal(ctx, project, conversationID, origin, nil, name, arguments)
}

// CallTaskToolWithGoal applies the selected task queue to this request only.
func (c *Client) CallTaskToolWithGoal(ctx context.Context, project, conversationID string, origin agent.Origin, goal *string, name string, arguments json.RawMessage) (storage.ToolResult, error) {
	var result storage.ToolResult
	var projectMessageID int64
	if name == "project_message_thread" {
		var input struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(arguments, &input) != nil || input.ID <= 0 {
			return result, fmt.Errorf("project message thread requires a positive message ID")
		}
		projectMessageID = input.ID
	}
	var delegation *TaskDelegateInput
	var contractBefore *storage.Task
	var contractInput storage.TaskDelegationUpdate
	if name == "task_update_delegation" {
		var input struct {
			ID string `json:"id"`
			storage.TaskDelegationUpdate
		}
		if json.Unmarshal(arguments, &input) != nil {
			return result, fmt.Errorf("invalid delegation contract arguments")
		}
		before, err := c.readDelegationForUpdate(ctx, project, input.ID)
		if err != nil {
			return result, err
		}
		contractBefore, contractInput = &before, input.TaskDelegationUpdate
	}
	var requesterTarget *storage.RegisteredTargetIdentity
	var requesterTaskID string
	requesterCreation := false
	if name == "task_create" || name == "task_delegate" {
		var input struct {
			NotifyResults bool `json:"notify_results"`
		}
		if json.Unmarshal(arguments, &input) != nil {
			return result, fmt.Errorf("invalid task creation arguments")
		}
		if input.NotifyResults {
			if err := c.RequireRequesterSubscriptions(ctx); err != nil {
				return result, err
			}
			var err error
			requesterTarget, err = c.RequesterTarget(ctx, project, origin)
			if err != nil {
				return result, err
			}
			requesterCreation = name == "task_create"
		}
	}
	if name == "task_set_requester" {
		var input struct {
			ID    string `json:"id"`
			Clear bool   `json:"clear"`
		}
		if json.Unmarshal(arguments, &input) != nil || !canonicalDelegationID(input.ID) {
			return result, fmt.Errorf("requester tool requires a canonical task ID")
		}
		if err := c.RequireRequesterSubscriptions(ctx); err != nil {
			return result, err
		}
		if !input.Clear {
			var err error
			requesterTarget, err = c.RequesterTarget(ctx, project, origin)
			if err != nil {
				return result, err
			}
		}
		requesterTaskID = input.ID
	}
	if name == "task_delegate" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(arguments, &fields) != nil || fields == nil {
			return result, fmt.Errorf("invalid task delegation arguments")
		}
		input := TaskDelegateInput{}
		if json.Unmarshal(fields["target_project"], &input.TargetProject) != nil || json.Unmarshal(fields["title"], &input.Title) != nil {
			return result, fmt.Errorf("task delegation requires title and target_project")
		}
		if raw, explicit := fields["parent"]; explicit {
			var parent string
			if json.Unmarshal(raw, &parent) != nil || !canonicalDelegationID(parent) {
				return result, fmt.Errorf("delegation parent must be a canonical non-nil UUID")
			}
			input.ParentID = &parent
		} else {
			conversation, err := c.Conversation(ctx, project, conversationID)
			if err != nil {
				return result, err
			}
			if conversation == nil || conversation.ID != conversationID || conversation.ProjectIdent != projectid.NormalizeIdent(project) || conversation.Owner != origin || !canonicalDelegationID(conversation.TaskID) {
				return result, fmt.Errorf("delegation requires the current conversation's native owner and parent")
			}
			input.ParentID = &conversation.TaskID
			fields["parent"], _ = json.Marshal(conversation.TaskID)
			arguments, _ = json.Marshal(fields)
		}
		input.TargetProject = projectid.NormalizeIdent(input.TargetProject)
		input.Requester = requesterTarget
		delegation = &input
	}
	err := c.doTaskGoal(ctx, "POST", conversationPath(project, conversationID)+"/tools", struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{name, arguments}, &origin, goal, &result)
	if err == nil && name == "project_message_thread" && !result.IsError {
		raw, parseErr := extractSingleTextJSON(result)
		if parseErr != nil {
			return storage.ToolResult{}, fmt.Errorf("invalid project message thread response: %w", parseErr)
		}
		var thread storage.ProjectMessageThread
		if json.Unmarshal(raw, &thread) != nil {
			return storage.ToolResult{}, fmt.Errorf("invalid project message thread response")
		}
		if err := validateProjectMessageThread(projectid.NormalizeIdent(project), projectMessageID, &thread); err != nil {
			return storage.ToolResult{}, err
		}
	}
	if contractBefore != nil {
		if err != nil {
			return result, fmt.Errorf("delegation contract not confirmed; inspect both projects before retrying: %w", err)
		}
		if !result.IsError {
			var receipt storage.TaskDelegationResult
			raw, parseErr := extractSingleTextJSON(result)
			if parseErr != nil || json.Unmarshal(raw, &receipt) != nil {
				return storage.ToolResult{}, fmt.Errorf("delegation contract receipt invalid; inspect both projects before retrying")
			}
			if err := validateDelegationUpdateReceipt(&receipt, projectid.NormalizeIdent(project), *contractBefore, contractInput); err != nil {
				return storage.ToolResult{}, err
			}
		}
	}
	if requesterCreation {
		if err != nil {
			return result, fmt.Errorf("requester creation not confirmed; inspect the task before retrying: %w", err)
		}
		if !result.IsError {
			raw, parseErr := extractSingleTextJSON(result)
			if parseErr != nil {
				return storage.ToolResult{}, fmt.Errorf("requester receipt invalid; inspect the task before retrying")
			}
			task, ackErr := taskAcknowledgement(raw, project, "")
			if ackErr != nil || !canonicalDelegationID(task.ID) {
				return storage.ToolResult{}, fmt.Errorf("requester creation scope not confirmed; inspect the task before retrying")
			}
			if err := requesterAcknowledgement(raw, requesterTarget); err != nil {
				return storage.ToolResult{}, err
			}
		}
	}
	if requesterTaskID != "" {
		if err != nil {
			return result, fmt.Errorf("requester not confirmed; inspect the task before retrying: %w", err)
		}
		if !result.IsError {
			raw, parseErr := extractSingleTextJSON(result)
			if parseErr != nil {
				return storage.ToolResult{}, fmt.Errorf("requester receipt invalid; inspect the task before retrying")
			}
			if _, err := taskAcknowledgement(raw, project, requesterTaskID); err != nil {
				return storage.ToolResult{}, fmt.Errorf("requester resource not confirmed; inspect the task before retrying")
			}
			if err := requesterAcknowledgement(raw, requesterTarget); err != nil {
				return storage.ToolResult{}, err
			}
		}
	}
	if delegation != nil {
		if err != nil {
			return result, fmt.Errorf("delegation not confirmed; inspect both projects before retrying: %w", err)
		}
		if !result.IsError {
			var receipt storage.TaskDelegationResult
			raw, parseErr := extractSingleTextJSON(result)
			if parseErr != nil || json.Unmarshal(raw, &receipt) != nil {
				return storage.ToolResult{}, fmt.Errorf("delegation receipt invalid; inspect both projects before retrying")
			}
			if err := validateDelegationReceipt(&receipt, projectid.NormalizeIdent(project), *delegation); err != nil {
				return storage.ToolResult{}, err
			}
		}
	}
	return result, err
}

func extractSingleTextJSON(result storage.ToolResult) (json.RawMessage, error) {
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		return nil, fmt.Errorf("tool result must contain exactly one text item")
	}
	raw := json.RawMessage(result.Content[0].Text)
	if !json.Valid(raw) {
		return nil, fmt.Errorf("tool result text is not valid JSON")
	}
	return raw, nil
}
