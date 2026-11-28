package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func invokeTaskToolAtGoal(t *testing.T, a *api, actor *agent.Origin, conversationID, goal, body string, includeGoal bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.SetPathValue("ident", "tasks")
	r.SetPathValue("conversation", conversationID)
	r.Header.Set("X-Agent-Session-Id", actor.SessionID)
	r.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
	r.Header.Set("X-Agent-Provider", actor.Provider)
	r.Header.Set("X-Agent-Os", actor.OS)
	if includeGoal {
		r.Header.Set("X-Task-Goal", goal)
	}
	w := httptest.NewRecorder()
	a.callTaskTool(w, r)
	return w
}

func decodeGoalToolResult(t *testing.T, response *httptest.ResponseRecorder) storage.ToolResult {
	t.Helper()
	var result storage.ToolResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 {
		t.Fatalf("task tool HTTP%d: %s (%v)", response.Code, response.Body.String(), err)
	}
	return result
}

func TestTaskToolsCarrySelectedGoalThroughListCreateAndClaim(t *testing.T) {
	ctx := context.Background()
	store, _ := taskAPI(t)
	actor := apiActor()
	goal := "Release"
	otherGoal := "Other"
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Conversation scope", Reporter: actor.SessionID}, actor)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := store.CreateConversation(ctx, "tasks", parent.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	releaseTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Release queue", Reporter: "user", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Other queue", Reporter: "user", Goal: &otherGoal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unscopedTask, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Unscoped queue", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(store, "unused")
	list := invokeTaskToolAtGoal(t, a, actor, conversation.ID, "  Release  ", `{"name":"task_list","arguments":{}}`, true)
	result := decodeGoalToolResult(t, list)
	if result.IsError {
		t.Fatalf("selected list: %+v", result)
	}
	var listed []storage.TaskSummary
	if err := json.Unmarshal([]byte(result.Content[0].Text), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != releaseTask.ID {
		t.Fatalf("selected queue returned %+v; want only %s", listed, releaseTask.ID)
	}
	unscoped := invokeTaskToolAtGoal(t, a, actor, conversation.ID, "", `{"name":"task_list","arguments":{}}`, true)
	result = decodeGoalToolResult(t, unscoped)
	if result.IsError {
		t.Fatalf("unscoped list: %+v", result)
	}
	listed = nil
	if err := json.Unmarshal([]byte(result.Content[0].Text), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != parent.ID && listed[1].ID != parent.ID || listed[0].ID != unscopedTask.ID && listed[1].ID != unscopedTask.ID {
		t.Fatalf("explicit unscoped queue returned %+v", listed)
	}
	missingHeader := invokeTaskToolAtGoal(t, a, actor, conversation.ID, "", `{"name":"task_list","arguments":{}}`, false)
	result = decodeGoalToolResult(t, missingHeader)
	if result.IsError {
		t.Fatalf("missing-header list: %+v", result)
	}
	listed = nil
	if err := json.Unmarshal([]byte(result.Content[0].Text), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("missing X-Task-Goal exposed non-default queues: %+v", listed)
	}

	created := invokeTaskToolAtGoal(t, a, actor, conversation.ID, goal, `{"name":"task_create","arguments":{"title":"Inherited goal","parent":"`+parent.ID+`"}}`, true)
	result = decodeGoalToolResult(t, created)
	if result.IsError {
		t.Fatalf("selected task create: %+v", result)
	}
	var createdTask storage.Task
	if err := json.Unmarshal([]byte(result.Content[0].Text), &createdTask); err != nil || createdTask.Goal == nil || *createdTask.Goal != goal {
		t.Fatalf("created task did not inherit selected goal: %+v err=%v", createdTask, err)
	}
	unscopedCreate := invokeTaskToolAtGoal(t, a, actor, conversation.ID, "", `{"name":"task_create","arguments":{"title":"Inherited unscoped goal","parent":"`+parent.ID+`"}}`, false)
	result = decodeGoalToolResult(t, unscopedCreate)
	if result.IsError {
		t.Fatalf("missing-header task create: %+v", result)
	}
	createdTask = storage.Task{}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &createdTask); err != nil || createdTask.Goal != nil {
		t.Fatalf("missing header did not inherit unscoped goal: %+v err=%v", createdTask, err)
	}

	wrongClaim := invokeTaskToolAtGoal(t, a, actor, conversation.ID, otherGoal, `{"name":"task_claim","arguments":{"id":"`+releaseTask.ID+`"}}`, true)
	if result := decodeGoalToolResult(t, wrongClaim); !result.IsError {
		t.Fatalf("wrong selected goal claimed task: %+v", result)
	}
	missingClaim := invokeTaskToolAtGoal(t, a, actor, conversation.ID, "", `{"name":"task_claim","arguments":{"id":"`+releaseTask.ID+`"}}`, false)
	if result := decodeGoalToolResult(t, missingClaim); !result.IsError {
		t.Fatalf("missing selected goal claimed task: %+v", result)
	}
	correctClaim := invokeTaskToolAtGoal(t, a, actor, conversation.ID, goal, `{"name":"task_claim","arguments":{"id":"`+releaseTask.ID+`"}}`, true)
	if result := decodeGoalToolResult(t, correctClaim); result.IsError {
		t.Fatalf("matching selected goal did not claim task: %+v", result)
	}
	unscopedClaim := invokeTaskToolAtGoal(t, a, actor, conversation.ID, "", `{"name":"task_claim","arguments":{"id":"`+unscopedTask.ID+`"}}`, false)
	if result := decodeGoalToolResult(t, unscopedClaim); result.IsError {
		t.Fatalf("missing header did not select unscoped claim queue: %+v", result)
	}
	if current, err := store.Task(ctx, "tasks", otherTask.ID); err != nil || current.Task.Status != "todo" {
		t.Fatalf("other goal task changed: %+v %v", current, err)
	}
}

func TestSelectedTaskToolGoalNormalizesAndRejectsAmbiguity(t *testing.T) {
	header := make(http.Header)
	header.Add("X-Task-Goal", "  Ship  ")
	selected, err := selectedTaskGoal(header)
	if err != nil || selected == nil || *selected != "Ship" {
		t.Fatalf("normalized goal=%v err=%v", selected, err)
	}
	empty := make(http.Header)
	empty.Set("X-Task-Goal", "")
	selected, err = selectedTaskGoal(empty)
	if err != nil || selected == nil || *selected != "" {
		t.Fatalf("explicit unscoped goal=%v err=%v", selected, err)
	}
	ambiguous := make(http.Header)
	ambiguous.Add("X-Task-Goal", "Ship")
	ambiguous.Add("X-Task-Goal", "Other")
	missing, err := selectedTaskGoal(make(http.Header))
	if err != nil || missing == nil || *missing != "" {
		t.Fatalf("missing goal header did not select unscoped: %v err=%v", missing, err)
	}
	if _, err := selectedTaskGoal(ambiguous); err == nil {
		t.Fatal("multiple selected goals were accepted")
	}
	invalid := make(http.Header)
	tooLong := strings.Repeat("x", 129)
	invalid.Set("X-Task-Goal", tooLong)
	if _, err := selectedTaskGoal(invalid); err == nil {
		t.Fatal("invalid selected goal was accepted")
	}
}
