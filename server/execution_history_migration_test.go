package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestExecutionHistoryRouteReadsMigratedRustRuns(t *testing.T) {
	ctx := context.Background()
	filename := filepath.Join(t.TempDir(), "gateway.db")
	schema, err := os.ReadFile("../storage/testdata/task_status_legacy.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('history-project','discord','',0);
 INSERT INTO execution_runs
		(id, project_ident, trigger, dedup_key, status, created_at, finished_at, output)
		VALUES ('retained-run', 'history-project', 'manual', 'history-key', 'completed', 10, 20, 'retained output');
		INSERT INTO execution_attempts (run_id, position, body)
		VALUES ('retained-run', 2, '{"sequence":2}'), ('retained-run', 1, '{"sequence":1}');`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := storage.Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	handler, err := NewHandler(store, "history-test-key")
	if err != nil {
		t.Fatal(err)
	}
	get := func(query string) []storage.ExecutionRun {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/execution/runs"+query, nil)
		request.Header.Set("Authorization", "Bearer history-test-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var runs []storage.ExecutionRun
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &runs) != nil || runs == nil {
			t.Fatalf("history status=%d body=%s", response.Code, response.Body.String())
		}
		return runs
	}
	runs := get("?project=history-project")
	if len(runs) != 1 || runs[0].ID != "retained-run" || runs[0].Output != "retained output" || runs[0].UpdatedAt != 20 {
		t.Fatalf("unexpected migrated history: %+v", runs)
	}
	attempts := runs[0].Attempts
	if len(attempts) != 2 || string(attempts[0]) != `{"sequence":1}` || string(attempts[1]) != `{"sequence":2}` {
		t.Fatalf("unexpected attempts: %s", attempts)
	}
	if runs := get("?project="); len(runs) != 0 {
		t.Fatalf("explicit empty project matched history: %+v", runs)
	}
}
