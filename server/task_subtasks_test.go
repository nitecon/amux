package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestTaskSubtasksHTTPCreateListAndPublishParentAtomically(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "destination", ChannelName: "destination", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	parent, err := store.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Parent", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	request := func(method, path, body string, actor *storage.AgentOrigin) (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer task-key")
		req.Header.Set("Content-Type", "application/json")
		if actor != nil {
			req.Header.Set("X-Agent-Session-Id", actor.SessionID)
			req.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
			req.Header.Set("X-Agent-Provider", actor.Provider)
			req.Header.Set("X-Agent-Os", actor.OS)
			req.Header.Set("X-Agent-Id", "worker")
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return response, data
	}
	base := "/v1/projects/tasks/tasks/" + parent.ID + "/subtasks"
	createdResponse, data := request(http.MethodPost, base, `{"title":"  Cross-project child ","target_project_ident":"destination","specification":"target work","labels":["os:linux"]}`, apiActor())
	if createdResponse.StatusCode != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createdResponse.StatusCode, data)
	}
	var child storage.Task
	if err := json.Unmarshal(data, &child); err != nil {
		t.Fatal(err)
	}
	if child.ProjectIdent != "destination" || child.Title != "Cross-project child" || child.Details == nil || *child.Details != "target work" || child.Parent == nil || child.Parent.ID != parent.ID || child.Parent.ProjectIdent != "tasks" {
		t.Fatalf("created subtask mismatch: %+v", child)
	}
	listResponse, data := request(http.MethodGet, base, "", nil)
	var children []storage.Task
	if listResponse.StatusCode != http.StatusOK || json.Unmarshal(data, &children) != nil || len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("list status=%d children=%+v body=%s", listResponse.StatusCode, children, data)
	}
	if _, body := request(http.MethodPost, base, `{"title":"bad","unexpected":true}`, nil); !strings.Contains(string(body), "invalid request") {
		t.Fatalf("unknown body field was accepted: %s", body)
	}
	if missing, body := request(http.MethodGet, "/v1/projects/destination/tasks/"+parent.ID+"/subtasks", "", nil); missing.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "parent task not found") {
		t.Fatalf("cross-project parent lookup status=%d body=%s", missing.StatusCode, body)
	}
	events, err := store.TaskEvents(context.Background(), storage.EventQuery{TaskID: &child.ID})
	if err != nil || len(events) != 1 || events[0].Kind != "task_created" {
		t.Fatalf("creation events=%+v err=%v", events, err)
	}
	var eventTask storage.Task
	if err := json.Unmarshal(events[0].Task, &eventTask); err != nil || eventTask.Parent == nil || eventTask.Parent.ID != parent.ID {
		t.Fatalf("creation event did not include parent link: %+v err=%v", eventTask, err)
	}
}
