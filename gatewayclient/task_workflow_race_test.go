package gatewayclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestResumeTaskDoesNotReopenWorkClosedAfterItsReviewSnapshot(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "resume-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "A decision that another user closes", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reason := "Review the proposed action"
	if _, err := s.UpdateTaskWorkflow(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "user_review", ReviewAction: &reason, Policy: storage.TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(s, "workflow-race")
	if err != nil {
		t.Fatal(err)
	}
	var snapshots atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != taskPath("demo")+"/"+task.ID || snapshots.Add(1) != 1 {
			handler.ServeHTTP(w, r)
			return
		}
		old := httptest.NewRecorder()
		handler.ServeHTTP(old, r)
		// Another user's close commits after the review GET captured its row,
		// before the delayed snapshot is delivered to ResumeTask.
		if _, err := s.UpdateTaskWorkflow(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "done", Policy: storage.TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
			t.Errorf("concurrent close: %v", err)
		}
		for key, values := range old.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(old.Code)
		_, _ = w.Write(old.Body.Bytes())
	}))
	defer host.Close()
	client, err := New(host.URL, "workflow-race", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ResumeTask(ctx, "demo", task.ID)
	var conflict *HTTPError
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
		t.Fatalf("stale resume must conflict, got %v", err)
	}
	current, err := s.Task(ctx, "demo", task.ID)
	if err != nil || current == nil || current.Task.Status != "done" || current.Task.DoneAt == nil {
		t.Fatalf("stale review action reopened completed work: %+v %v", current, err)
	}
}

func TestParkTaskDoesNotReopenWorkClosedBeforeItsWrite(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "park-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Active work closed by another user", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(s, "park-race")
	if err != nil {
		t.Fatal(err)
	}
	var patches atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && patches.Add(1) == 1 {
			if _, err := s.UpdateTaskWorkflow(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "done", Policy: storage.TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
				t.Errorf("concurrent close: %v", err)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer host.Close()
	client, err := New(host.URL, "park-race", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ParkTask(ctx, "demo", task.ID, "Review this active task")
	var conflict *HTTPError
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
		t.Fatalf("stale park must conflict, got %v", err)
	}
	current, err := s.Task(ctx, "demo", task.ID)
	if err != nil || current == nil || current.Task.Status != "done" || current.Task.DoneAt == nil {
		t.Fatalf("stale park reopened completed work: %+v %v", current, err)
	}
}
