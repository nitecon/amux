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

func TestHumanTaskMutationsDoNotReplayAfterCommittedConnectionLoss(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "uncertain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	api, err := server.NewHandler(s, "uncertain-fixture")
	if err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			api.ServeHTTP(w, r)
			return
		}
		posts.Add(1)
		result := httptest.NewRecorder()
		api.ServeHTTP(result, r)
		if result.Code < 200 || result.Code >= 300 {
			w.WriteHeader(result.Code)
			_, _ = w.Write(result.Body.Bytes())
			return
		}
		// The durable operation succeeded, but the caller receives no HTTP
		// acknowledgement. Replaying this POST would create another row.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack committed response: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer host.Close()
	client, err := New(host.URL, "uncertain-fixture", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	if task, err := client.CreateTask(ctx, "demo", TaskCreateInput{Title: "Keep an unconfirmed receipt"}); task != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("lost create acknowledgement: %+v %v", task, err)
	}
	rows, err := client.TaskList(ctx, "demo", TaskListOptions{AllGoals: true}, nil, nil)
	if err != nil || len(rows) != 1 || posts.Load() != 1 {
		t.Fatalf("create was lost or replayed: %+v posts=%d err=%v", rows, posts.Load(), err)
	}
	if comment, err := client.AddTaskComment(ctx, "demo", rows[0].ID, "Keep this draft after connection loss"); comment != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("lost comment acknowledgement: %+v %v", comment, err)
	}
	detail, err := s.Task(ctx, "demo", rows[0].ID)
	if err != nil || detail == nil || len(detail.Comments) != 1 || posts.Load() != 2 {
		t.Fatalf("comment was lost or replayed: %+v posts=%d err=%v", detail, posts.Load(), err)
	}
	if detail.Comments[0].AuthorType != "user" || detail.Comments[0].Origin != nil {
		t.Fatalf("human operation changed provenance: %+v", detail.Comments[0])
	}
}
