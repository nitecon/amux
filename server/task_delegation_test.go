package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/storage"
)

func TestTaskDelegationHTTPAndToolLifecycle(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	if err := store.InsertProject(ctx, storage.Project{Ident: "target", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Parent", Reporter: actor.SessionID, RootReason: stringPtr("user-directed"), Details: stringPtr("Delegation scope")}, actor)
	if err != nil {
		t.Fatal(err)
	}

	caps := taskRequest(handler, http.MethodGet, "/v1/tasks/capabilities", "", nil)
	if caps.Code != http.StatusOK || strings.TrimSpace(caps.Body.String()) != `{"changeset_confirmation":1,"guided_confirmation":1,"native_worker_assignment":1,"requester_subscriptions":1,"task_hierarchy":1}` {
		t.Fatalf("capabilities %d %s", caps.Code, caps.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"target_project_ident": "target", "title": "Handoff", "parent_task_id": parent.ID, "specification": "literal $(text)"})
	created := taskRequest(handler, http.MethodPost, "/v1/projects/tasks/tasks/delegate", string(body), actor)
	if created.Code != http.StatusOK {
		t.Fatalf("delegation %d %s", created.Code, created.Body.String())
	}
	var receipt storage.TaskDelegationResult
	if err := json.Unmarshal(created.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.SourceTask.Parent == nil || receipt.SourceTask.Parent.ID != parent.ID || receipt.SourceTask.ProjectIdent != "tasks" || receipt.TargetTask.ProjectIdent != "target" || receipt.TargetTask.Parent == nil || receipt.TargetTask.Parent.ID != receipt.SourceTask.ID {
		t.Fatalf("invalid delegation receipt: %+v", receipt)
	}

	conversation, err := store.CreateConversation(ctx, "tasks", parent.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	api := newAPI(store, "unused")
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"task_delegate","arguments":{"title":"Tool handoff","target_project":"target"}}`))
	request.SetPathValue("ident", "tasks")
	request.SetPathValue("conversation", conversation.ID)
	request.Header.Set("X-Agent-Session-Id", actor.SessionID)
	request.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
	request.Header.Set("X-Agent-Provider", actor.Provider)
	request.Header.Set("X-Agent-Os", actor.OS)
	response := httptest.NewRecorder()
	api.callTaskTool(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("MCP tool HTTP %d: %s", response.Code, response.Body.String())
	}
	var toolResult mcpstdio.Result
	if err := json.Unmarshal(response.Body.Bytes(), &toolResult); err != nil || toolResult.IsError {
		t.Fatalf("MCP tool result %+v err=%v", toolResult, err)
	}
	var toolReceipt storage.TaskDelegationResult
	if err := json.Unmarshal([]byte(toolResult.Content[0].Text), &toolReceipt); err != nil {
		t.Fatalf("MCP delegation receipt %q: %v", toolResult.Content[0].Text, err)
	}
	if toolReceipt.SourceTask.Parent == nil || toolReceipt.SourceTask.Parent.ID != parent.ID {
		t.Fatalf("MCP did not default to conversation task: %+v", toolReceipt.SourceTask.Parent)
	}
}

func TestTaskDelegationAllowsHumanRoot(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "target", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	response := taskRequest(handler, http.MethodPost, "/v1/projects/tasks/tasks/delegate", `{"target_project_ident":"target","title":"User handoff","details":"literal details"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("human root delegation %d: %s", response.Code, response.Body.String())
	}
	var receipt storage.TaskDelegationResult
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.SourceTask.Parent != nil || receipt.SourceTask.OwnerOrigin != nil || receipt.SourceTask.Details == nil || *receipt.SourceTask.Details != "literal details" {
		t.Fatalf("unexpected human root receipt: %+v", receipt.SourceTask)
	}
}
