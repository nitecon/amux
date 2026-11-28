package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestNativeTaskClaimCannotBypassUnboundGoal(t *testing.T) {
	s, _ := taskAPI(t)
	ctx := context.Background()
	goal := "selected-goal-required"
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Goal work", Reporter: "user", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	anchor, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Unscoped conversation anchor", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversationWithGoal(ctx, "tasks", anchor.ID, actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := newAPI(s, "unused")
	w := invokeTaskTool(t, a, actor, c.ID, `{"name":"task_claim","arguments":{"id":"`+task.ID+`"}}`)
	var result storage.ToolResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.IsError {
		t.Fatalf("goal claim bypass: HTTP%d result=%+v err=%v", w.Code, result, err)
	}
	loaded, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || loaded.Task.Status != "todo" || loaded.Task.OwnerOrigin != nil {
		t.Fatalf("goal task claimed: %+v %v", loaded, err)
	}
}
