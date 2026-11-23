package gatewayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestNavigationReadsProjectsTasksDetailsAndConversationPages(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "navigation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	goal, details := "separate-goal", "Task specification"
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Browse goal work", Reporter: "user", Goal: &goal, Details: &details}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	if _, err := s.AddTaskComment(ctx, "demo", task.ID, "user", "user", "Inspect history", nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.CreateConversationWithGoal(ctx, "demo", task.ID, &owner, &goal); err != nil {
			t.Fatal(err)
		}
	}
	h, err := server.NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	c, err := New(host.URL, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := c.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Ident != "demo" {
		t.Fatalf("projects %+v %v", projects, err)
	}
	tasks, err := c.Tasks(ctx, "demo", []string{"in_progress"})
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID || tasks[0].Status != "in_progress" {
		t.Fatalf("goal task missing from browser %+v %v", tasks, err)
	}
	events, err := c.TaskEvents(ctx, "demo")
	if err != nil || len(events) == 0 || events[0].ProjectIdent != "demo" {
		t.Fatalf("project task events %+v %v", events, err)
	}
	detail, err := c.TaskDetail(ctx, "demo", task.ID)
	if err != nil || detail.Task.ID != task.ID || detail.Task.Details == nil || *detail.Task.Details != details || len(detail.Comments) != 1 || detail.CommentCount != 1 {
		t.Fatalf("detail %+v %v", detail, err)
	}
	first, err := c.TaskConversations(ctx, "demo", task.ID, "", 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first conversation page %+v %v", first, err)
	}
	next, err := c.TaskConversations(ctx, "demo", task.ID, first[0].ID, 1)
	if err != nil || len(next) != 1 || next[0].ID <= first[0].ID {
		t.Fatalf("next conversation page %+v %v", next, err)
	}
	projectPage, err := c.ProjectConversations(ctx, "demo", first[0].ID, 1)
	if err != nil || len(projectPage) != 1 || projectPage[0].ID != next[0].ID || projectPage[0].TaskID != task.ID || projectPage[0].TaskTitle != task.Title {
		t.Fatalf("project conversation page %+v %v", projectPage, err)
	}
	otherPage, err := c.ProjectConversations(ctx, "other", "", 10)
	if err != nil || len(otherPage) != 0 {
		t.Fatalf("project conversation scope %+v %v", otherPage, err)
	}
	standalone, err := c.CreateConversation(ctx, "demo", "", owner)
	if err != nil || standalone == nil || standalone.TaskID != "" || standalone.ProjectIdent != "demo" {
		t.Fatalf("standalone conversation %+v %v", standalone, err)
	}
	reopened, err := c.Conversation(ctx, "demo", standalone.ID)
	if err != nil || *reopened != *standalone {
		t.Fatalf("reopened standalone conversation %+v %v", reopened, err)
	}
	all, err := c.ProjectConversations(ctx, "demo", "", 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("project conversations after standalone creation %+v %v", all, err)
	}
	bad, err := New(host.URL, "wrong-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Projects(ctx); err == nil {
		t.Fatal("unauthenticated project inventory accepted")
	}
	if _, err := bad.ProjectConversations(ctx, "demo", "", 10); err == nil {
		t.Fatal("unauthenticated conversation inventory accepted")
	}
}

func TestTaskDetailValidatesTaskScopeAndPreservesEnvelope(t *testing.T) {
	const id = "legacy-task-id"
	validEnvelope := `{"id":"` + id + `","project_ident":"demo","title":"Detail","status":"todo","specification":"fallback specification","comments":[{"id":"comment-1","task_id":"` + id + `","author":"user","author_type":"user","content":"saved comment"}],"comment_count":7}`
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validEnvelope))
	}))
	defer host.Close()
	client, err := New(host.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := client.TaskDetail(context.Background(), "demo", id)
	if err != nil || detail.Task.ID != id || detail.Task.Details == nil || *detail.Task.Details != "fallback specification" || len(detail.Comments) != 1 || detail.Comments[0].Content != "saved comment" || detail.CommentCount != 7 {
		t.Fatalf("task detail envelope changed: %+v %v", detail, err)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"wrong project", `{"id":"` + id + `","project_ident":"other","status":"todo"}`},
		{"wrong id", `{"id":"another-legacy-id","project_ident":"demo","status":"todo"}`},
		{"unknown status", `{"id":"` + id + `","project_ident":"demo","status":"unknown"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer badHost.Close()
			badClient, err := New(badHost.URL, "secret", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := badClient.TaskDetail(context.Background(), "demo", id); err == nil {
				t.Fatal("accepted task detail outside requested scope or with unknown status")
			}
		})
	}
}
