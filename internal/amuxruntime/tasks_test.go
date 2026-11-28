package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

const (
	taskTestSession  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	taskTestInstance = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	taskTestID       = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

func TestTasksRequireExplicitServerProjectAndWriteActor(t *testing.T) {
	base := []string{"--server", "http://127.0.0.1:7913", "--project", "demo"}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing server", []string{"--project", "demo", "list"}, "--server and --project are required"},
		{"missing project", []string{"--server", "http://127.0.0.1:7913", "list"}, "--server and --project are required"},
		{"write missing actor", append(append([]string(nil), base...), "claim", taskTestID), "requires a native agent origin"},
		{"partial actor", append(append([]string(nil), base...), "--session", taskTestSession, "claim", taskTestID), "requires --session, --instance, --provider, and --os"},
		{"actor human conflict", append(append([]string(nil), base...), "--human", "--session", taskTestSession, "--instance", taskTestInstance, "--provider", "codex", "--os", "linux", "claim", taskTestID), "mutually exclusive"},
		{"all goals write", append(append([]string(nil), base...), "--human", "--all-goals", "done", taskTestID), "readonly list only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, diag bytes.Buffer
			err := runTasks(context.Background(), tc.args, &out, &diag, func(string) string { return "key" })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("unexpected stdout: %s", out.String())
			}
		})
	}
}

func TestTasksReadonlyListNeedsNoActorAndWritesOneJSONResult(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/tasks" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		if got := r.URL.Query().Get("goal"); got != "release" {
			t.Errorf("goal query = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("authorization missing")
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer host.Close()
	var out, diag bytes.Buffer
	args := []string{"--server", host.URL, "--project", "demo", "--goal", "release", "list", "--include-stale"}
	if err := runTasks(context.Background(), args, &out, &diag, func(string) string { return "test-key" }); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "[]\n" {
		t.Fatalf("stdout = %q", got)
	}
	if diag.Len() != 0 || calls.Load() != 1 {
		t.Fatalf("stderr=%q requests=%d", diag.String(), calls.Load())
	}
}

func TestTasksListHelpIsUsefulWithoutConnectionAndWritesNoJSON(t *testing.T) {
	var out, diag bytes.Buffer
	if err := runTasks(context.Background(), []string{"list", "--help"}, &out, &diag, func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("help wrote protocol stdout: %q", out.String())
	}
	if !strings.Contains(diag.String(), "--status STATUS") || !strings.Contains(diag.String(), "--server URL") {
		t.Fatalf("incomplete help: %q", diag.String())
	}
}

func TestTasksStartedRequiresExactActiveNativeOwnerAndOnlyComments(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		owner        *agent.Origin
		wantErr      bool
	}{
		{"exact active owner", "in_progress", &agent.Origin{SessionID: taskTestSession, InstanceID: taskTestInstance, Provider: "codex", OS: "linux"}, false},
		{"unowned", "todo", nil, true},
		{"other instance", "in_progress", &agent.Origin{SessionID: taskTestSession, InstanceID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Provider: "codex", OS: "linux"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(storage.Task{ID: taskTestID, ProjectIdent: "demo", Status: tc.status, OwnerOrigin: tc.owner})
					return
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
					writes.Add(1)
					var body struct {
						Content    string `json:"content"`
						Author     string `json:"author"`
						AuthorType string `json:"author_type"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if !strings.Contains(body.Content, "recorded as a comment, not an atomic gateway lifecycle transition") {
						t.Errorf("started comment = %q", body.Content)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"comment-1","task_id":"` + taskTestID + `","author":"` + taskTestSession + `","author_type":"agent","origin":{"session_id":"` + taskTestSession + `","instance_id":"` + taskTestInstance + `","provider":"codex","os":"linux"},"content":"started"}`))
					return
				}
				t.Errorf("unexpected request %s %s", r.Method, r.URL)
				http.Error(w, "unexpected", http.StatusBadRequest)
			}))
			defer host.Close()
			args := []string{"--server", host.URL, "--project", "demo", "--session", taskTestSession, "--instance", taskTestInstance, "--provider", "codex", "--os", "linux", "started", taskTestID}
			var out, diag bytes.Buffer
			err := runTasks(context.Background(), args, &out, &diag, func(string) string { return "test-key" })
			if tc.wantErr {
				if err == nil || writes.Load() != 0 {
					t.Fatalf("error=%v writes=%d", err, writes.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if writes.Load() != 1 || !json.Valid(bytes.TrimSpace(out.Bytes())) {
				t.Fatalf("writes=%d stdout=%q", writes.Load(), out.String())
			}
		})
	}
}

func TestTasksFailedMutationDoesNotRetryOrLeakServerBody(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("credential=server-secret"))
	}))
	defer host.Close()
	args := []string{"--server", host.URL, "--project", "demo", "--human", "comment", taskTestID, "hello"}
	var out, diag bytes.Buffer
	err := runTasks(context.Background(), args, &out, &diag, func(string) string { return "test-key" })
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("request count = %d, want one", calls.Load())
	}
	if strings.Contains(err.Error()+diag.String()+out.String(), "server-secret") || out.Len() != 0 {
		t.Fatalf("leaked response or stdout: err=%v stderr=%q stdout=%q", err, diag.String(), out.String())
	}
}

func TestTaskReleasePreflightsNativeOwnerAndDoneReadsBeforeUpdate(t *testing.T) {
	actor := agent.Origin{SessionID: taskTestSession, InstanceID: taskTestInstance, Provider: "codex", OS: "linux"}
	for _, tc := range []struct {
		name       string
		operation  string
		status     string
		owner      *agent.Origin
		wantWrites int
	}{
		{name: "release unowned task", operation: "release", status: "todo", wantWrites: 0},
		{name: "release exact active owner", operation: "release", status: "in_progress", owner: &actor, wantWrites: 1},
		{name: "done reads but permits other status and owner", operation: "done", status: "todo", wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var methods []string
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				methods = append(methods, r.Method)
				w.Header().Set("Content-Type", "application/json")
				status := tc.status
				if r.Method == http.MethodPatch {
					if tc.operation == "done" {
						status = "done"
					} else {
						status = "todo"
					}
				}
				_ = json.NewEncoder(w).Encode(storage.Task{ID: taskTestID, ProjectIdent: "demo", Status: status, OwnerOrigin: tc.owner})
			}))
			defer host.Close()
			args := []string{"--server", host.URL, "--project", "demo", "--session", actor.SessionID, "--instance", actor.InstanceID, "--provider", actor.Provider, "--os", actor.OS, tc.operation, taskTestID}
			var out, diag bytes.Buffer
			err := runTasks(context.Background(), args, &out, &diag, func(string) string { return "test-key" })
			if tc.operation == "release" && tc.wantWrites == 0 {
				if err == nil || !strings.Contains(err.Error(), "exact owning execution") {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(methods) != 1+tc.wantWrites || methods[0] != http.MethodGet {
				t.Fatalf("request methods = %v", methods)
			}
			if tc.wantWrites == 1 && methods[1] != http.MethodPatch {
				t.Fatalf("write method = %v", methods)
			}
		})
	}
}

func TestTaskOperationParserHonorsTerminator(t *testing.T) {
	pos, err := parseOperation(context.Background(), commandFlags("tasks comment", ioDiscard{}), []string{taskTestID, "--", "--literal"})
	if err != nil || len(pos) != 2 || pos[1] != "--literal" {
		t.Fatalf("pos=%v err=%v", pos, err)
	}
	if _, err = parseOperation(context.Background(), commandFlags("tasks comment", ioDiscard{}), []string{taskTestID, "--unknown"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if _, err = storage.CanonicalTaskID("00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("zero UUID accepted")
	}
}

func TestTasksHumanClaimRejectsBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer host.Close()
	var out, diag bytes.Buffer
	err := runTasks(context.Background(), []string{"--server", host.URL, "--project", "demo", "--human", "claim", taskTestID}, &out, &diag, func(string) string { return "test-key" })
	if err == nil || !strings.Contains(err.Error(), "requires a native agent origin") || calls.Load() != 0 {
		t.Fatalf("error=%v requests=%d", err, calls.Load())
	}
}

func TestTasksUnknownCommentFlagRejectsBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer host.Close()
	var out, diag bytes.Buffer
	err := runTasks(context.Background(), []string{"--server", host.URL, "--project", "demo", "--human", "comment", taskTestID, "--unknown"}, &out, &diag, func(string) string { return "test-key" })
	if err == nil || !strings.Contains(err.Error(), "unknown flag") || calls.Load() != 0 || out.Len() != 0 {
		t.Fatalf("error=%v requests=%d stdout=%q", err, calls.Load(), out.String())
	}
}

func TestChangesetParsingUsesStorageValidationAndPreservesRawJSON(t *testing.T) {
	for _, invalid := range []string{"[]", `["", "file.go"]`, `["bad\u0000path"]`, `{"path":"file.go"}`, ""} {
		if _, err := parseChangeset(invalid); err == nil {
			t.Errorf("accepted invalid changeset %q", invalid)
		}
	}
	raw := ` [" path/file.go ","second.go"] `
	got, err := parseChangeset(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != raw {
		t.Fatalf("changeset bytes changed: %q != %q", got, raw)
	}
	if absent, err := parseOptionalChangeset(nil, false); err != nil || absent != nil {
		t.Fatalf("absent changeset = %q, %v", absent, err)
	}
	empty := ""
	if _, err := parseOptionalChangeset(&empty, true); err == nil {
		t.Fatal("explicit empty changeset accepted")
	}
	null := "null"
	if got, err := parseOptionalChangeset(&null, true); err != nil || string(got) != null {
		t.Fatalf("literal null = %q, %v", got, err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
