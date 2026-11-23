package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/storage"
)

func delegationUpdateAPIForTest(t *testing.T) (*storage.Store, http.Handler, *storage.AgentOrigin, *storage.Task, *storage.TaskDelegationResult) {
	t.Helper()
	store, handler := taskAPI(t)
	ctx := context.Background()
	if err := store.InsertProject(ctx, storage.Project{Ident: "target", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	rootReason, details := "user-directed", "Delegation scope"
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Parent", Reporter: actor.SessionID, RootReason: &rootReason, Details: &details}, actor)
	if err != nil {
		t.Fatal(err)
	}
	delegation, err := store.DelegateTask(ctx, storage.TaskDelegationInput{
		SourceProject: "tasks", TargetProject: "target", Title: "Initial", Reporter: actor.SessionID, ParentID: &parent.ID,
	}, actor)
	if err != nil {
		t.Fatal(err)
	}
	return store, handler, actor, parent, delegation
}

func TestDelegationUpdateHTTPAllowsTextEditsButKeepsOSOwnerCheck(t *testing.T) {
	store, handler, _, _, delegation := delegationUpdateAPIForTest(t)
	path := "/v1/projects/tasks/tasks/" + delegation.SourceTask.ID + "/delegation"
	textEdit := taskRequest(handler, http.MethodPatch, path, `{"title":"Editor update","description":"plain text edit"}`, nil)
	if textEdit.Code != http.StatusOK {
		t.Fatalf("ordinary human text edit was blocked: %d %s", textEdit.Code, textEdit.Body.String())
	}
	var receipt storage.TaskDelegationResult
	if err := json.Unmarshal(textEdit.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.SourceTask.Title != "Editor update (DELEGATED)" || receipt.TargetTask.Title != "Editor update" || receipt.SourceTask.Description == nil || *receipt.SourceTask.Description != "plain text edit" || receipt.TargetTask.Description == nil || *receipt.TargetTask.Description != "plain text edit" {
		t.Fatalf("text metadata was not mirrored: %+v", receipt)
	}

	beforeSource, err := store.Task(context.Background(), "tasks", delegation.SourceTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeTarget, err := store.Task(context.Background(), "target", delegation.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	ownerlessOSChange := taskRequest(handler, http.MethodPatch, path, `{"labels":["os:windows"]}`, nil)
	if ownerlessOSChange.Code != http.StatusConflict {
		t.Fatalf("OS requirement edit without owner returned %d: %s", ownerlessOSChange.Code, ownerlessOSChange.Body.String())
	}
	afterSource, err := store.Task(context.Background(), "tasks", delegation.SourceTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterTarget, err := store.Task(context.Background(), "target", delegation.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeSource.Task.Title != afterSource.Task.Title || beforeTarget.Task.Title != afterTarget.Task.Title || len(beforeSource.Task.Labels) != len(afterSource.Task.Labels) || len(beforeTarget.Task.Labels) != len(afterTarget.Task.Labels) {
		t.Fatalf("rejected OS edit changed linked tasks: source=%+v target=%+v", afterSource.Task, afterTarget.Task)
	}
}

func TestTaskUpdateDelegationToolUsesCurrentConversationFence(t *testing.T) {
	store, _, actor, parent, delegation := delegationUpdateAPIForTest(t)
	conversation, err := store.CreateConversation(context.Background(), "tasks", parent.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "unused")
	args := `{"name":"task_update_delegation","arguments":{"id":"` + delegation.SourceTask.ID + `","title":"MCP update","details":"literal spec","labels":[]}}`
	response := invokeTaskTool(t, a, actor, conversation.ID, args)
	if response.Code != http.StatusOK {
		t.Fatalf("task tool HTTP %d: %s", response.Code, response.Body.String())
	}
	var result mcpstdio.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.IsError {
		t.Fatalf("task tool result=%+v err=%v", result, err)
	}
	var receipt storage.TaskDelegationResult
	if len(result.Content) != 1 || json.Unmarshal([]byte(result.Content[0].Text), &receipt) != nil {
		t.Fatalf("task tool receipt: %+v", result)
	}
	if receipt.SourceTask.Title != "MCP update (DELEGATED)" || receipt.TargetTask.Title != "MCP update" || receipt.TargetTask.Details == nil || *receipt.TargetTask.Details != "literal spec" || len(receipt.SourceTask.Labels) != 0 {
		t.Fatalf("task tool did not return mirrored update: %+v", receipt)
	}

	current := *actor
	current.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := store.ResumeConversation(context.Background(), "tasks", conversation.ID, actor.InstanceID, &current); err != nil {
		t.Fatal(err)
	}
	stale := invokeTaskTool(t, a, actor, conversation.ID, args)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale conversation update status=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestDelegationUpdateToolSchemaHasNullableFields(t *testing.T) {
	tool, ok := taskToolByName("task_update_delegation")
	if !ok {
		t.Fatal("task_update_delegation is not advertised")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"title", "description", "details", "specification", "labels"} {
		var property struct {
			Type json.RawMessage `json:"type"`
		}
		if err := json.Unmarshal(schema.Properties[field], &property); err != nil || len(property.Type) == 0 || string(property.Type) != `"string"` && string(property.Type) != `"array"` && string(property.Type) != `["string","null"]` && string(property.Type) != `["array","null"]` {
			t.Errorf("%s schema does not allow source-compatible null/no-op: %s err=%v", field, schema.Properties[field], err)
		}
	}
}
