package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestReorderTasksSelectedGoalAndResponseFiltering(t *testing.T) {
	store, h := taskAPI(t)
	ctx := context.Background()
	selected, other := "selected", "other"
	first, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "selected", Reporter: "user", Goal: &selected}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "other", Reporter: "user", Goal: &other}, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := func(path, body string, actor *storage.AgentOrigin, goal string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer task-key")
		req.Header.Set("Content-Type", "application/json")
		if actor != nil {
			req.Header.Set("X-Agent-Session-Id", actor.SessionID)
			req.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
			req.Header.Set("X-Agent-Provider", actor.Provider)
			req.Header.Set("X-Agent-OS", actor.OS)
		}
		if goal != "" {
			req.Header.Set("X-Task-Goal", goal)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}

	filtered := request("/v1/projects/tasks/tasks/reorder?status=todo", `{"order":["`+first.ID+`","`+second.ID+`"]}`, apiActor(), selected)
	if filtered.Code != http.StatusOK {
		t.Fatalf("reorder %d %s", filtered.Code, filtered.Body.String())
	}
	var tasks []storage.TaskSummary
	if err := json.Unmarshal(filtered.Body.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != first.ID {
		t.Fatalf("agent response was not filtered to selected goal: %+v", tasks)
	}
	wrongGoal := request("/v1/projects/tasks/tasks/reorder?status=in_progress", `{"order":["`+first.ID+`","`+second.ID+`"]}`, apiActor(), selected)
	if wrongGoal.Code != http.StatusConflict {
		t.Fatalf("wrong selected goal status %d %s", wrongGoal.Code, wrongGoal.Body.String())
	}
	for _, id := range []string{first.ID, second.ID} {
		detail, err := store.Task(ctx, "tasks", id)
		if err != nil || detail.Task.Status != "todo" || detail.Task.OwnerOrigin != nil {
			t.Fatalf("failed reorder changed %s: %+v, %v", id, detail, err)
		}
	}
}

func TestReorderTasksRejectsDuplicateQueryFields(t *testing.T) {
	_, h := taskAPI(t)
	for _, path := range []string{
		"/v1/projects/tasks/tasks/reorder?status=todo&status=done",
		"/v1/projects/tasks/tasks/reorder?status=todo&goal=one&goal=two",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"order":[]}`))
		req.Header.Set("Authorization", "Bearer task-key")
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Errorf("duplicate query fields accepted for %s: %d %s", path, res.Code, res.Body.String())
		}
	}
}
