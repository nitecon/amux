package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestUpdateTaskMetadataUsesGuardedAPI(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "edit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Old", Reporter: "user", Labels: []string{"task-review", "review-of:source"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	client, err := New(host.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	title := "Renamed"
	got, err := client.UpdateTask(ctx, "demo", task.ID, storage.TaskMetadataUpdate{Title: &title, Specification: json.RawMessage(`"Saved plan"`)})
	if err != nil || got.Title != title || got.Details == nil || *got.Details != "Saved plan" {
		t.Fatalf("save: %+v %v", got, err)
	}
	got, err = client.UpdateTask(ctx, "demo", task.ID, storage.TaskMetadataUpdate{Specification: json.RawMessage(`null`)})
	if err != nil || got.Details != nil {
		t.Fatalf("clear specification: %+v %v", got, err)
	}
	_, err = client.UpdateTask(ctx, "demo", task.ID, storage.TaskMetadataUpdate{Labels: json.RawMessage(`[]`)})
	var response *HTTPError
	if !errors.As(err, &response) || response.Status != 409 {
		t.Fatalf("review labels bypassed: %v", err)
	}
}

func TestUpdateTaskDoesNotRetryFailedSave(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPatch || r.URL.Path != "/v1/projects/demo/tasks/id" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Agent-Session-Id") != "" {
			t.Error("human edit invented agent identity")
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"secret should not be exposed"}`))
	}))
	defer host.Close()
	client, _ := New(host.URL, "secret", nil)
	title := "Draft"
	_, err := client.UpdateTask(context.Background(), "demo", "id", storage.TaskMetadataUpdate{Title: &title})
	if err == nil || requests.Load() != 1 || err.Error() != "gateway request failed (HTTP 502)" {
		t.Fatalf("failed write retried/leaked: %d %v", requests.Load(), err)
	}
}
