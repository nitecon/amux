package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/mcpstdio"
	"github.com/nitecon/amux/storage"
)

func TestHumanCanSetCurrentRequesterWithoutImpersonatingOwner(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "User task", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	watcher := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	generation := uuid.NewString()
	if _, err := store.RegisterTarget(ctx, "tasks", storage.TargetRegistration{Origin: watcher, Generation: generation}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	identity := storage.RegisteredTargetIdentity{Origin: watcher, Generation: generation}
	body, err := json.Marshal(map[string]any{"requester": identity})
	if err != nil {
		t.Fatal(err)
	}
	response := taskRequest(handler, http.MethodPut, "/v1/projects/tasks/tasks/"+task.ID+"/requester", string(body), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("human requester update %d: %s", response.Code, response.Body.String())
	}
	var updated storage.Task
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	var acknowledged storage.RegisteredTargetIdentity
	if err := json.Unmarshal(updated.Requester, &acknowledged); err != nil || acknowledged != identity {
		t.Fatalf("requester acknowledgement=%+v err=%v", acknowledged, err)
	}
	if updated.Reporter != "user" || updated.OwnerOrigin != nil || updated.OwnerAgentID != nil || updated.Status != "todo" {
		t.Fatalf("human requester update impersonated/changed task ownership: %+v", updated)
	}
	cleared := taskRequest(handler, http.MethodPut, "/v1/projects/tasks/tasks/"+task.ID+"/requester", `{"requester":null}`, nil)
	if cleared.Code != http.StatusOK {
		t.Fatalf("human explicit clear %d: %s", cleared.Code, cleared.Body.String())
	}
	if err := json.Unmarshal(cleared.Body.Bytes(), &updated); err != nil || len(updated.Requester) == 0 || string(updated.Requester) != "null" || updated.Reporter != "user" || updated.OwnerOrigin != nil {
		t.Fatalf("human clear changed task audit/ownership: %+v err=%v", updated, err)
	}
}

func TestTaskCreateAcceptsTypedRequesterForHumanSelectedExecution(t *testing.T) {
	store, handler := taskAPI(t)
	watcher := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	generation := uuid.NewString()
	if _, err := store.RegisterTarget(context.Background(), "tasks", storage.TargetRegistration{Origin: watcher, Generation: generation}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	requester := storage.RegisteredTargetIdentity{Origin: watcher, Generation: generation}
	body, err := json.Marshal(map[string]any{
		"title": "Human task with watcher", "root_reason": "user-directed", "specification": "Subscribe this execution", "requester": requester,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := taskRequest(handler, http.MethodPost, "/v1/projects/tasks/tasks", string(body), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("create with requester %d: %s", response.Code, response.Body.String())
	}
	var created storage.Task
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var acknowledged storage.RegisteredTargetIdentity
	if json.Unmarshal(created.Requester, &acknowledged) != nil || acknowledged != requester {
		t.Fatalf("create requester acknowledgement=%+v raw=%s", acknowledged, created.Requester)
	}
	if created.Reporter != "user" || created.OwnerOrigin != nil || created.OwnerAgentID != nil {
		t.Fatalf("requester selection changed human task authorship/ownership: %+v", created)
	}
}

func TestTaskRequesterEndpointRequiresExplicitRequesterField(t *testing.T) {
	_, handler := taskAPI(t)
	for _, body := range []string{`{}`, `{"requester":null,"extra":true}`, `{"requester":{"origin":{"session_id":"x"},"generation":"bad"}}`} {
		response := taskRequest(handler, http.MethodPut, "/v1/projects/tasks/tasks/00000000-0000-4000-8000-000000000001/requester", body, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("body %s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}
}

func TestNativeTaskToolsSubscribeOnlyTheCurrentExecution(t *testing.T) {
	store, _ := taskAPI(t)
	ctx := context.Background()
	actor := apiActor()
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Parent", Reporter: actor.SessionID, RootReason: stringPtr("user-directed"), Details: stringPtr("MCP requester scope")}, actor)
	if err != nil {
		t.Fatal(err)
	}
	generation := uuid.NewString()
	if _, err := store.RegisterTarget(ctx, "tasks", storage.TargetRegistration{Origin: *actor, Generation: generation}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.CreateConversation(ctx, "tasks", parent.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "unused")
	call := func(body string) mcpstdio.Result {
		t.Helper()
		response := invokeTaskTool(t, a, actor, conversation.ID, body)
		if response.Code != http.StatusOK {
			t.Fatalf("task tool HTTP %d: %s", response.Code, response.Body.String())
		}
		var result mcpstdio.Result
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.IsError {
			t.Fatalf("task tool result %+v err=%v", result, err)
		}
		return result
	}
	created := call(`{"name":"task_create","arguments":{"title":"Subscribed child","parent":"` + parent.ID + `","specification":"MCP requester","notify_results":true}}`)
	var task storage.Task
	if len(created.Content) != 1 || json.Unmarshal([]byte(created.Content[0].Text), &task) != nil || task.ID == "" {
		t.Fatalf("invalid created task receipt: %+v", created)
	}
	var requester storage.RegisteredTargetIdentity
	if json.Unmarshal(task.Requester, &requester) != nil || requester.Origin != *actor || requester.Generation != generation {
		t.Fatalf("task_create did not subscribe exact execution: %s", task.Requester)
	}
	cleared := call(`{"name":"task_set_requester","arguments":{"id":"` + task.ID + `","clear":true}}`)
	var after storage.Task
	if len(cleared.Content) != 1 || json.Unmarshal([]byte(cleared.Content[0].Text), &after) != nil || len(after.Requester) == 0 || string(after.Requester) != "null" {
		t.Fatalf("task_set_requester did not explicitly clear subscription: %+v", cleared)
	}
}
