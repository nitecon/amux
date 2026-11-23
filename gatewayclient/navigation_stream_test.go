package gatewayclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestNavigationStreamResynchronizesAndCancels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "nav.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := server.NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	c, _ := New(host.URL, "key", nil)
	changes := make(chan struct{}, 8)
	done := make(chan error, 1)
	go func() { done <- c.WatchNavigation(ctx, func() error { changes <- struct{}{}; return nil }) }()
	select {
	case <-changes:
	case <-ctx.Done():
		t.Fatal("missing initial resync")
	}
	if err := s.InsertProject(ctx, storage.Project{Ident: "new-project", ChannelName: "new-project", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-ctx.Done():
		t.Fatal("missing committed project notification")
	}
	projects, err := c.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Ident != "new-project" {
		t.Fatalf("project not committed before notification: %+v %v", projects, err)
	}
	for _, topic := range []eventbus.Topic{eventbus.TaskChanged, eventbus.ConversationChanged, eventbus.PresenceChanged, eventbus.Tick} {
		s.EventBus().Publish(eventbus.Event{Topic: topic})
		select {
		case <-changes:
		case <-ctx.Done():
			t.Fatalf("missing %s", topic)
		}
	}
	if _, err := s.SetProjectArchived(ctx, "new-project", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-ctx.Done():
		t.Fatal("missing archive notification")
	}
	projects, err = c.Projects(ctx)
	if err != nil || len(projects) != 0 {
		t.Fatalf("archived inventory: %+v %v", projects, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stream did not stop")
	}
	bad, _ := New(host.URL, "wrong", nil)
	err = bad.WatchNavigation(context.Background(), func() error { return nil })
	var response *HTTPError
	if !errors.As(err, &response) || response.Status != 401 {
		t.Fatalf("auth: %v", err)
	}
}

func TestNavigationStreamRejectsInvalidProtocolAndPropagatesCallback(t *testing.T) {
	for _, first := range []string{"changed", "ready"} {
		t.Run(first, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_ = wsjson.Write(ctx, conn, map[string]any{"version": 1, "type": first})
				<-conn.CloseRead(ctx).Done()
			}))
			defer host.Close()
			c, _ := New(host.URL, "key", nil)
			stop := errors.New("projection stopped")
			called := false
			err := c.WatchNavigation(ctx, func() error { called = true; return stop })
			if first == "ready" {
				if !called || !errors.Is(err, stop) {
					t.Fatalf("callback: %v %v", called, err)
				}
			} else if called || err == nil || Retryable(err) {
				t.Fatalf("bad protocol accepted: %v %v", called, err)
			}
		})
	}
}

func TestNavigationStreamServerShutdownDrainsConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "nav.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := server.NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	client, _ := New(host.URL, "key", nil)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- client.WatchNavigation(ctx, func() error { close(ready); return nil }) }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("stream not ready")
	}
	owner := h.(interface{ ShutdownStreams(context.Context) error })
	if err := owner.ShutdownStreams(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("closed stream: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stream not drained")
	}
}

func TestNavigationStreamSeesCommittedTaskMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "nav.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Old title", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	client, _ := New(host.URL, "key", nil)
	changes := make(chan struct{}, 4)
	done := make(chan error, 1)
	go func() { done <- client.WatchNavigation(ctx, func() error { changes <- struct{}{}; return nil }) }()
	defer func() { cancel(); <-done }()
	select {
	case <-changes:
	case <-ctx.Done():
		t.Fatal("stream not ready")
	}
	var result storage.Task
	if err := client.do(ctx, http.MethodPatch, "/v1/projects/demo/tasks/"+task.ID, map[string]any{"title": "New title"}, nil, &result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-ctx.Done():
		t.Fatal("metadata edit did not invalidate navigation")
	}
	current, err := client.Task(ctx, "demo", task.ID)
	if err != nil || current.Title != "New title" {
		t.Fatalf("metadata not committed before notification: %+v %v", current, err)
	}
}
