package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/nitecon/amux/storage"
)

func TestExecutionHistoryRouteOnFreshStoreReturnsEmptyArray(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	response := artifactWriteRequest(handler, http.MethodGet, "/v1/execution/runs", nil, nil)
	if response.Code != http.StatusOK || response.Body.String() != "[]\n" {
		t.Fatalf("fresh history response status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestExecutionHistoryRouteAuthorizationFiltersAndJSONAttempts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: "history", ChannelName: "history", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "history", Title: "History task", Reporter: "tester"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE execution_runs (
id TEXT PRIMARY KEY,project_ident TEXT NOT NULL REFERENCES projects(ident),task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
trigger TEXT NOT NULL,dedup_key TEXT UNIQUE NOT NULL,status TEXT NOT NULL,created_at INTEGER NOT NULL,finished_at INTEGER,
output TEXT NOT NULL DEFAULT '',started_at INTEGER,updated_at INTEGER,client TEXT,model TEXT,executor TEXT NOT NULL DEFAULT 'headless',
progress TEXT NOT NULL DEFAULT '',summary TEXT NOT NULL DEFAULT '',summary_source TEXT NOT NULL DEFAULT 'missing',session_key TEXT,last_sequence INTEGER NOT NULL DEFAULT 0);
CREATE TABLE execution_attempts(run_id TEXT NOT NULL REFERENCES execution_runs(id) ON DELETE CASCADE,position INTEGER NOT NULL,body TEXT NOT NULL,PRIMARY KEY(run_id,position));
INSERT INTO execution_runs(id,project_ident,task_id,trigger,dedup_key,status,created_at,finished_at,output,started_at,updated_at,client,model,executor,progress,summary,summary_source,session_key,last_sequence)
VALUES('active','history',?,'manual','active-key','running',10,NULL,'output',11,12,'codex','model-a','headless','working','progress summary','runtime',NULL,4);
INSERT INTO execution_runs(id,project_ident,task_id,trigger,dedup_key,status,created_at,finished_at,output,updated_at)
VALUES('complete','history',?,'manual','complete-key','completed',20,30,'done',30);
INSERT INTO execution_attempts VALUES('active',2,'{"n":2}');
INSERT INTO execution_attempts VALUES('active',1,'{"n":1}');`, task.ID, task.ID)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	unchangedTask, err := store.Task(ctx, "history", task.ID)
	if err != nil || unchangedTask.Task.Status != "todo" {
		t.Fatalf("run retirement changed task status: task=%+v err=%v", unchangedTask, err)
	}
	handler, err := NewHandler(store, "history-key")
	if err != nil {
		t.Fatal(err)
	}
	call := func(url string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		if authenticated {
			req.Header.Set("Authorization", "Bearer history-key")
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	if response := call("/v1/execution/runs", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", response.Code, response.Body.String())
	}
	response := call("/v1/execution/runs?project=history&task_id="+task.ID, true)
	if response.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", response.Code, response.Body.String())
	}
	var runs []storage.ExecutionRun
	if err := json.Unmarshal(response.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "complete" || runs[1].ID != "active" {
		t.Fatalf("history order/filter: %+v", runs)
	}
	retired := runs[1]
	if retired.Status != "retired" || retired.FinishedAt == nil || *retired.FinishedAt <= 12 || retired.UpdatedAt != *retired.FinishedAt {
		t.Fatalf("startup retirement: %+v", retired)
	}
	if retired.TaskTitle == nil || *retired.TaskTitle != "History task" || retired.StartedAt == nil || *retired.StartedAt != 11 || retired.Client == nil || *retired.Client != "codex" || retired.Model == nil || *retired.Model != "model-a" || retired.Executor != "headless" || retired.Progress != "Legacy executor removed; no work resumed" || retired.Summary != "progress summary" || retired.SummarySource != "runtime" || retired.LastSequence != 4 {
		t.Fatalf("current history fields: %+v", retired)
	}
	if len(retired.Attempts) != 2 || string(retired.Attempts[0]) != `{"n":1}` || string(retired.Attempts[1]) != `{"n":2}` {
		t.Fatalf("attempt order/content: %s", retired.Attempts)
	}
	emptyProject := call("/v1/execution/runs?project=", true)
	if emptyProject.Code != http.StatusOK || json.Unmarshal(emptyProject.Body.Bytes(), &runs) != nil || len(runs) != 0 {
		t.Fatalf("empty project filter status=%d runs=%+v body=%s", emptyProject.Code, runs, emptyProject.Body.String())
	}
}
