package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestNativeTaskOperationsPreserveGoalOriginAndGuidedConfirmation(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)
	actor := agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	goal, spec, reason := "release", "User requested this task.", "user-directed"
	raw, err := client.TaskCreate(ctx, "demo", TaskCreateInput{Title: "Native task", RootReason: &reason, Specification: &spec}, &actor, &goal)
	if err != nil {
		t.Fatal(err)
	}
	var task storage.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	if task.Goal == nil || *task.Goal != goal || task.OwnerOrigin == nil || *task.OwnerOrigin != actor || task.Reporter != actor.SessionID {
		t.Fatalf("create lost goal/origin: %+v", task)
	}
	list, err := client.TaskList(ctx, "demo", TaskListOptions{}, &actor, nil)
	if err != nil || len(list) != 0 {
		t.Fatalf("native list crossed goals: %+v %v", list, err)
	}
	list, err = client.TaskList(ctx, "demo", TaskListOptions{}, &actor, &goal)
	if err != nil || len(list) != 1 {
		t.Fatalf("selected list: %+v %v", list, err)
	}
	wrong := "other"
	if _, err := client.TaskUpdate(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "in_progress"}, &actor, &wrong); err == nil {
		t.Fatal("claim bypassed goal")
	}
	if _, err := client.TaskUpdate(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "in_progress"}, &actor, &goal); err != nil {
		t.Fatal(err)
	}
	comment, err := client.TaskComment(ctx, "demo", task.ID, "Agent comment", &actor)
	if err != nil {
		t.Fatal(err)
	}
	var posted storage.TaskComment
	if err := json.Unmarshal(comment, &posted); err != nil || posted.Author != actor.SessionID || posted.AuthorType != "agent" || posted.Origin == nil || *posted.Origin != actor {
		t.Fatalf("comment provenance: %+v %v", posted, err)
	}
	raw, err = client.TaskRead(ctx, "demo", task.ID, 1)
	if err != nil || !strings.Contains(string(raw), "Agent comment") {
		t.Fatalf("recent detail: %s %v", raw, err)
	}
	_, err = client.TaskUpdate(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "done"}, &actor, &goal)
	var response *HTTPError
	if !errors.As(err, &response) || response.Code != "task_confirmation_required" {
		t.Fatalf("guided error lost: %+v", err)
	}
	kind := "software"
	raw, err = client.TaskConfirm(ctx, "demo", task.ID, &kind, nil, &actor)
	if err != nil || !strings.Contains(string(raw), "task_set_changeset") {
		t.Fatalf("mutation confirm: %s %v", raw, err)
	}
	raw, err = client.TaskConfirm(ctx, "demo", task.ID, nil, json.RawMessage(`["main.go"]`), &actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &task); err != nil || task.Status != "done" || len(task.Changeset) != 1 {
		t.Fatalf("completion: %+v %v", task, err)
	}
}

func TestTypedTaskCreateAndHumanCommentUseCurrentServerAPI(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "typed-tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)

	task, err := client.CreateTask(ctx, "demo", TaskCreateInput{Title: "Human task"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID == "" || task.ProjectIdent != "demo" || task.Status != "todo" || task.OwnerOrigin != nil {
		t.Fatalf("unexpected human task acknowledgement: %+v", task)
	}

	content := "A human comment\nwith preserved lines."
	comment, err := client.AddTaskComment(ctx, "demo", task.ID, content)
	if err != nil {
		t.Fatal(err)
	}
	if comment.ID == "" || comment.TaskID != task.ID || comment.Author != "user" || comment.AuthorType != "user" || comment.Content != content || comment.Origin != nil {
		t.Fatalf("unexpected human comment acknowledgement: %+v", comment)
	}
}

func TestTaskErrorCodesAreBoundedAndWhitelisted(t *testing.T) {
	for _, body := range []string{`{"code":"unexpected-secret","error":"secret"}`, `{"code":"task_confirmation_required","error":"secret"}`, `{"code":"changeset_confirmation_required"}`, `{"code":"task_confirmation_required","error":"` + strings.Repeat("x", 5000) + `"}`} {
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(body))
		}))
		client, _ := New(host.URL, "secret", nil)
		_, err := client.TaskUpdate(context.Background(), "demo", "id", storage.TaskWorkflowUpdate{Status: "done"}, nil, nil)
		host.Close()
		var response *HTTPError
		if !errors.As(err, &response) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error: %v", err)
		}
		expected := ""
		if len(body) <= 4096 && !strings.Contains(body, "unexpected-secret") {
			var envelope struct{ Code string }
			json.Unmarshal([]byte(body), &envelope)
			expected = envelope.Code
		}
		if response.Code != expected {
			t.Fatalf("code=%q want=%q", response.Code, expected)
		}
	}
}

func TestTaskOperationsRejectMismatchedSuccessAcknowledgement(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"wrong-task","project_ident":"other-project","status":"in_progress","task_id":"wrong-task"}`))
	}))
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)
	ctx := context.Background()
	if _, err := client.TaskRead(ctx, "demo", "requested", 0); err == nil {
		t.Error("read accepted wrong task")
	}
	if _, err := client.TaskUpdate(ctx, "demo", "requested", storage.TaskWorkflowUpdate{Status: "in_progress"}, nil, nil); err == nil {
		t.Error("update accepted wrong task")
	}
	if _, err := client.TaskCreate(ctx, "demo", TaskCreateInput{Title: "New"}, nil, nil); err == nil {
		t.Error("create accepted wrong project")
	}
	if _, err := client.CreateTask(ctx, "demo", TaskCreateInput{Title: "New"}); err == nil {
		t.Error("typed create accepted wrong project")
	}
	if _, err := client.TaskComment(ctx, "demo", "requested", "Comment", nil); err == nil {
		t.Error("comment accepted wrong task")
	}
	if _, err := client.TaskConfirm(ctx, "demo", "requested", nil, json.RawMessage(`null`), nil); err == nil {
		t.Error("confirmation accepted wrong task")
	}
}

func TestAddTaskCommentRejectsMalformedAcknowledgements(t *testing.T) {
	const taskID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	validID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing id", `{"task_id":"` + taskID + `","author":"user","author_type":"user","content":"note"}`},
		{"zero id", `{"id":"00000000-0000-0000-0000-000000000000","task_id":"` + taskID + `","author":"user","author_type":"user","content":"note"}`},
		{"invalid id", `{"id":"not-a-uuid","task_id":"` + taskID + `","author":"user","author_type":"user","content":"note"}`},
		{"different content", `{"id":"` + validID + `","task_id":"` + taskID + `","author":"user","author_type":"user","content":"changed"}`},
		{"agent origin", `{"id":"` + validID + `","task_id":"` + taskID + `","author":"user","author_type":"user","content":"note","origin":{"session_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","instance_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd","provider":"codex","os":"linux"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			if _, err := client.AddTaskComment(context.Background(), "demo", taskID, "note"); err == nil {
				t.Fatal("accepted malformed comment acknowledgement")
			}
		})
	}
}

func TestTypedTaskMutationsDoNotRetryHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*Client) error
	}{
		{"create", func(c *Client) error {
			_, err := c.CreateTask(context.Background(), "demo", TaskCreateInput{Title: "New"})
			return err
		}},
		{"comment", func(c *Client) error {
			_, err := c.AddTaskComment(context.Background(), "demo", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "note")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer host.Close()
			client, _ := New(host.URL, "secret", nil)
			err := tc.call(client)
			var response *HTTPError
			if !errors.As(err, &response) || response.Status != http.StatusInternalServerError {
				t.Fatalf("expected typed server error, got %v", err)
			}
			if requests != 1 {
				t.Fatalf("request count=%d, want 1", requests)
			}
		})
	}
}

func TestTaskUpdateRejectsIgnoredRankAndLabels(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"requested","project_ident":"demo","status":"todo","rank":9,"labels":["os:linux"]}`))
	}))
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)
	rank := int64(-4)
	for _, update := range []storage.TaskWorkflowUpdate{{TaskMetadataUpdate: storage.TaskMetadataUpdate{Rank: &rank}}, {TaskMetadataUpdate: storage.TaskMetadataUpdate{Labels: json.RawMessage(`["os:windows"]`)}}} {
		if _, err := client.TaskUpdate(context.Background(), "demo", "requested", update, nil, nil); err == nil {
			t.Errorf("ignored update acknowledged: %+v", update)
		}
	}
}
