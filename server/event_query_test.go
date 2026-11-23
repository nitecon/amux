package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/nitecon/amux/storage"
)

func TestEventQueryRejectsDuplicateKnownScalarsAndInvalidCursorsOnBothRoutes(t *testing.T) {
	handler := eventQueryHTTPFixture(t)
	invalid := []string{
		"after_event_id=bad",
		"after_event_id=1&after_event_id=2",
		"after_event_id=9223372036854775808",
		"project=a&project=b",
		"task_id=a&task_id=b",
		"project=%ZZ",
	}
	for _, route := range []string{"/v1/task-events", "/v1/execution/runs"} {
		for _, query := range invalid {
			t.Run(strings.TrimPrefix(route, "/v1/")+"?"+query, func(t *testing.T) {
				response := eventQueryRequest(handler, route+"?"+query)
				if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid event query") {
					t.Fatalf("%s?%s => %d %s", route, query, response.Code, response.Body)
				}
			})
		}
	}
	for _, route := range []string{"/v1/task-events", "/v1/execution/runs"} {
		response := eventQueryRequest(handler, route+"?unrecognized=x&unrecognized=y")
		if response.Code != http.StatusOK {
			t.Errorf("unknown repeated query fields should be ignored: %s => %d %s", route, response.Code, response.Body)
		}
	}
}

func TestEventQueryPreservesEmptyProjectFilterBehavior(t *testing.T) {
	handler := eventQueryHTTPFixture(t)
	eventsResponse := eventQueryRequest(handler, "/v1/task-events?project=")
	var events []storage.TaskEvent
	if err := json.Unmarshal(eventsResponse.Body.Bytes(), &events); eventsResponse.Code != http.StatusOK || err != nil || len(events) != 2 {
		t.Fatalf("empty task-event project filter status=%d events=%+v err=%v body=%s", eventsResponse.Code, events, err, eventsResponse.Body)
	}
	runsResponse := eventQueryRequest(handler, "/v1/execution/runs?project=")
	var runs []storage.ExecutionRun
	if err := json.Unmarshal(runsResponse.Body.Bytes(), &runs); runsResponse.Code != http.StatusOK || err != nil || len(runs) != 0 {
		t.Fatalf("empty execution-history project filter status=%d runs=%+v err=%v body=%s", runsResponse.Code, runs, err, runsResponse.Body)
	}
}

func eventQueryHTTPFixture(t *testing.T) http.Handler {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "event-query.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, project := range []string{"alpha", "beta"} {
		if _, err := store.AppendTaskEvent(ctx, storage.TaskEvent{Kind: "created", ProjectIdent: project, Task: json.RawMessage(`{"id":"task-` + project + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE execution_runs (
 id TEXT PRIMARY KEY, project_ident TEXT NOT NULL, task_id TEXT,
 trigger TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL,
 finished_at INTEGER, output TEXT NOT NULL DEFAULT ''
);
INSERT INTO execution_runs(id,project_ident,trigger,status,created_at) VALUES('history-one','history','manual','completed',1);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "event-query-key")
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func eventQueryRequest(handler http.Handler, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer event-query-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
