package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

func submitWorkMCPTool() storage.TaskTool {
	return storage.TaskTool{Name: "submit_work", Description: "Submit the listed changed files for the currently owned task. Commit and publish them, or open a pull request, according to local work submission settings, then post an agent comment with the result. For a recovered task commit, supply resume_commit and the exact original path list.", InputSchema: json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"},"message":{"type":"string"},"paths":{"type":"array","description":"Changed files to submit (modified, added or deleted), relative to the project or absolute","items":{"type":"string"}},"resume_commit":{"type":"string"}},"required":["task_id","message","paths"],"additionalProperties":false}`)}
}

func callSubmitWorkMCP(ctx context.Context, client *gatewayclient.Client, project, conversationID, root string, origin agent.Origin, goal *string, preferencesPath string, args json.RawMessage) (storage.ToolResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: "invalid submit_work arguments"}}, IsError: true}, nil
	}
	for name := range fields {
		if name != "task_id" && name != "message" && name != "paths" && name != "resume_commit" {
			return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: "unknown submit_work argument: " + name}}, IsError: true}, nil
		}
	}
	var request struct {
		TaskID       string   `json:"task_id"`
		Message      string   `json:"message"`
		Paths        []string `json:"paths"`
		ResumeCommit string   `json:"resume_commit"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: "invalid submit_work arguments"}}, IsError: true}, nil
	}
	pathsRaw, pathsPresent := fields["paths"]
	if pathsPresent && string(pathsRaw) != "null" {
		paths, err := worksubmission.DecodePaths(pathsRaw)
		if err != nil {
			return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: err.Error()}}, IsError: true}, nil
		}
		request.Paths = paths[:0]
		for _, path := range paths {
			if strings.TrimSpace(path) != "" {
				request.Paths = append(request.Paths, path)
			}
		}
	}
	if request.TaskID == "" || request.Message == "" || len(request.Paths) == 0 {
		return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: "task_id, message, and paths are required"}}, IsError: true}, nil
	}
	if err := worksubmission.ValidateRoot(root); err != nil {
		return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: err.Error()}}, IsError: true}, nil
	}
	receipt, err := worksubmission.SubmitTask(ctx, client, worksubmission.Input{Project: project, ConversationID: conversationID, Root: root, TaskID: request.TaskID, Message: request.Message, Paths: request.Paths, ResumeCommit: request.ResumeCommit, Origin: &origin, Goal: goal}, preferencesPath)
	if err != nil {
		return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: err.Error()}}, IsError: true}, nil
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return storage.ToolResult{}, err
	}
	return storage.ToolResult{Content: []storage.ToolText{{Type: "text", Text: string(encoded)}}}, nil
}
