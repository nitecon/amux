package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/worksubmission"
)

func TestSubmitWorkToolIsLocalMCPOnlyAndNeedsPinnedDirectory(t *testing.T) {
	tool := submitWorkMCPTool()
	if tool.Name != "submit_work" || !strings.Contains(string(tool.InputSchema), `"task_id"`) || !strings.Contains(string(tool.InputSchema), `"resume_commit"`) {
		t.Fatalf("unexpected local work submission tool: %+v", tool)
	}
	for _, remoteTool := range server.AgentTaskTools() {
		if remoteTool.Name == "submit_work" {
			t.Fatal("local filesystem submission was added to the server tool catalog")
		}
	}
	args := json.RawMessage(`{"task_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","message":"submit","paths":["main.go"]}`)
	result, err := callSubmitWorkMCP(context.Background(), nil, "demo", "conversation", "", agent.Origin{}, nil, "", args)
	if err != nil || !result.IsError || !strings.Contains(result.Content[0].Text, "absolute checkout") {
		t.Fatalf("local MCP tool did not require an explicit pinned checkout: %+v %v", result, err)
	}
}
func TestSubmitWorkMCPPathsMatchSourceListContract(t *testing.T) {
	paths, err := worksubmission.DecodePaths(json.RawMessage(`["a file.txt","-dash.txt",""," \t "]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a file.txt", "-dash.txt", "", " \t "}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("decoded paths=%q, want exact source-list paths %q", paths, want)
	}

	tool := submitWorkMCPTool()
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			Items       struct {
				Type string `json:"type"`
			} `json:"items"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["paths"].Type != "array" || schema.Properties["paths"].Items.Type != "string" ||
		schema.Properties["paths"].Description != "Changed files to submit (modified, added or deleted), relative to the project or absolute" ||
		!reflect.DeepEqual(schema.Required, []string{"task_id", "message", "paths"}) || schema.Properties["resume_commit"].Type != "string" {
		t.Fatalf("submit_work schema differs from source contract: %s", tool.InputSchema)
	}
}
