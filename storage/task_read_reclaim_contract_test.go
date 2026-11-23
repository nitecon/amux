package storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	_ "modernc.org/sqlite"
)

func TestTaskReadsDoNotReclaimUntilClockTick(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "task-read-reclaim.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "parity", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "parity", Title: "stale legacy work", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Seed the pre-native legacy ownership shape through a separate connection:
	// an assigned in-progress row with no owner_origin_json.
	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyDB.Close()
	if _, err := legacyDB.ExecContext(ctx, `UPDATE tasks SET status='in_progress', owner_agent_id='owner',
		started_at=0, updated_at=0 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}

	handler, err := server.NewHandler(store, "read-reclaim-key")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer read-reclaim-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", path, response.Code, response.Body.String())
		}
		return response
	}
	type taskView struct {
		ID           string  `json:"id"`
		Status       string  `json:"status"`
		OwnerAgentID *string `json:"owner_agent_id"`
	}
	type commentView struct {
		Author     string `json:"author"`
		AuthorType string `json:"author_type"`
	}
	type detailView struct {
		taskView
		Comments []commentView `json:"comments"`
	}
	listResponse := request("/v1/projects/parity/tasks")
	var listed []taskView
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != task.ID || listed[0].Status != "in_progress" || listed[0].OwnerAgentID == nil || *listed[0].OwnerAgentID != "owner" {
		t.Fatalf("task list read changed or hid stale work: %+v", listed)
	}
	detailResponse := request("/v1/projects/parity/tasks/" + task.ID)
	var before detailView
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &before); err != nil {
		t.Fatal(err)
	}
	if before.ID != task.ID || before.Status != "in_progress" || before.OwnerAgentID == nil || *before.OwnerAgentID != "owner" || len(before.Comments) != 0 {
		t.Fatalf("task detail read changed stale work: %+v", before)
	}

	definitions := append(storage.RouterTaskEventDefinitions(), storage.RouterPresenceEventDefinitions()...)
	definitions = append(definitions, plugins.ClockDefinition(), plugins.ClientHeartbeatDefinition())
	definitions = append(definitions, plugins.ActivityEventDefinitions()...)
	runtime, err := router.NewRuntime(store, store.EventBus(), definitions, []router.Plugin{
		plugins.PresencePlugin(store),
		plugins.ReclaimPlugin(store),
	}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tick, err := store.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{
		ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runtime.Run(runCtx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("router runtime: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("router runtime did not stop")
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		response := request("/v1/projects/parity/tasks/" + task.ID)
		var after detailView
		if err := json.Unmarshal(response.Body.Bytes(), &after); err != nil {
			t.Fatal(err)
		}
		if after.Status == "todo" {
			if after.OwnerAgentID != nil || len(after.Comments) != 1 || after.Comments[0].Author != "system" || after.Comments[0].AuthorType != "system" {
				t.Fatalf("clock tick produced unexpected reclaim result (tick %s): %+v", tick.Event.UID, after)
			}
			break
		}
		if after.Status != "in_progress" || after.OwnerAgentID == nil || *after.OwnerAgentID != "owner" || len(after.Comments) != 0 {
			t.Fatalf("task changed before clock controller reclaimed it: %+v", after)
		}
		if time.Now().After(deadline) {
			t.Fatal("clock tick did not reclaim stale legacy task")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
