package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestTasksCancelRequiresNativeOriginBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer host.Close()
	for _, args := range [][]string{
		{"--server", host.URL, "--project", "demo", "--human", "cancel", taskTestID, "--reason", "stop"},
		{"--server", host.URL, "--project", "demo", "cancel", taskTestID, "--reason", "stop"},
	} {
		var out, diag bytes.Buffer
		err := runTasks(context.Background(), args, &out, &diag, func(string) string { return "key" })
		if err == nil || !strings.Contains(err.Error(), "native agent origin") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid cancellation sent %d requests", requests.Load())
	}
}

func TestTasksCancelUsesNativeCancellationRouteWithoutPreflight(t *testing.T) {
	origin := agent.Origin{SessionID: taskTestSession, InstanceID: taskTestInstance, Provider: "codex", OS: "linux"}
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/tasks/"+taskTestID+"/cancellation" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-Agent-Session-Id") != origin.SessionID || r.Header.Get("X-Agent-Instance-Id") != origin.InstanceID {
			t.Error("native origin headers missing")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["reason"]) != `"superseded"` || string(body["changeset"]) != `null` {
			t.Errorf("body=%s", body)
		}
		response := map[string]any{
			"task":         map[string]any{"id": taskTestID, "project_ident": "demo", "status": "cancelled"},
			"cancellation": map[string]any{"task_id": taskTestID, "project_ident": "demo", "reason": "superseded", "prior_status": "todo", "reporter_origin": origin, "cancelled_at": 123, "executor_stop_confirmed": false},
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer host.Close()
	args := []string{"--server", host.URL, "--project", "demo", "--session", origin.SessionID, "--instance", origin.InstanceID, "--provider", origin.Provider, "--os", origin.OS, "cancel", taskTestID, "--reason", "superseded", "--changeset", "null"}
	var out, diag bytes.Buffer
	if err := runTasks(context.Background(), args, &out, &diag, func(string) string { return "key" }); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !json.Valid(bytes.TrimSpace(out.Bytes())) {
		t.Fatalf("calls=%d output=%q", calls.Load(), out.String())
	}
}

func TestTasksCancellationIsReadonlyAndRequiresNoActor(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/tasks/"+taskTestID+"/cancellation" {
			t.Errorf("request %s %s", r.Method, r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": taskTestID, "project_ident": "demo", "reason": "superseded", "prior_status": "todo", "reporter_origin": agent.Origin{SessionID: taskTestSession, InstanceID: taskTestInstance, Provider: "codex", OS: "linux"}, "cancelled_at": 123, "executor_stop_confirmed": false})
	}))
	defer host.Close()
	var out, diag bytes.Buffer
	args := []string{"--server", host.URL, "--project", "demo", "cancellation", taskTestID}
	if err := runTasks(context.Background(), args, &out, &diag, func(string) string { return "key" }); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(out.String(), `"task_id":"`+taskTestID+`"`) {
		t.Fatalf("calls=%d output=%q", calls.Load(), out.String())
	}
}

func TestTasksCancellationHelpShowsNativeCancelAndReadonlyRead(t *testing.T) {
	for _, tc := range []struct{ operation, usage string }{
		{"cancel", "cancel TASK_ID --reason TEXT [--changeset JSON]"},
		{"cancellation", "cancellation TASK_ID"},
	} {
		var out, diag bytes.Buffer
		if err := runTasks(context.Background(), []string{tc.operation, "--help"}, &out, &diag, func(string) string { return "" }); err != nil {
			t.Fatalf("%s help: %v", tc.operation, err)
		}
		if out.Len() != 0 || !strings.Contains(diag.String(), tc.usage) {
			t.Fatalf("%s help stdout=%q stderr=%q", tc.operation, out.String(), diag.String())
		}
	}
}

func TestHeadlessCancellationCLIUsesGatewayAliasAndKeyFallback(t *testing.T) {
	t.Run("unwrapped cancel validates before request", func(t *testing.T) {
		var requests atomic.Int32
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
		defer host.Close()
		state := filepath.Join(t.TempDir(), "absent-state")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, stderr, err := invokeTaskCLIProcessWithEnvAt(ctx, "", map[string]string{
			"AMUX_GATEWAY_KEY":       "fixture-key",
			"AMUX_STATE_DIR":         state,
			"AMUX_AGENT_SESSION_ID":  "",
			"AMUX_AGENT_INSTANCE_ID": "",
			"AMUX_AGENT_PROVIDER":    "",
			"AMUX_AGENT_OS":          "",
		}, []string{"tasks", "--project", "demo", "--gateway-url", host.URL}, "cancel", taskTestID, "--reason", "requested")
		if err == nil || !strings.Contains(stderr, "native agent origin") {
			t.Fatalf("unwrapped cancel err=%v stderr=%q", err, stderr)
		}
		if requests.Load() != 0 {
			t.Fatalf("invalid unwrapped cancellation sent %d requests", requests.Load())
		}
		if _, statErr := os.Stat(state); !os.IsNotExist(statErr) {
			t.Fatalf("invalid cancellation created state: %v", statErr)
		}
	})

	t.Run("actorless cancellation read", func(t *testing.T) {
		origin := agent.Origin{SessionID: taskTestSession, InstanceID: taskTestInstance, Provider: "codex", OS: "linux"}
		want := storage.Cancellation{
			TaskID: taskTestID, ProjectIdent: "demo", Reason: "requested", PriorStatus: "todo",
			ReporterOrigin: origin, CancelledAt: 123, ExecutorStopConfirmed: false,
		}
		var requests atomic.Int32
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/tasks/"+taskTestID+"/cancellation" {
				t.Errorf("request %s %s", r.Method, r.URL)
			}
			if r.Header.Get("Authorization") != "Bearer fixture-key" {
				t.Errorf("authorization=%q", r.Header.Get("Authorization"))
			}
			for key := range r.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-agent-") {
					t.Errorf("read sent agent identity header %q", key)
				}
			}
			_ = json.NewEncoder(w).Encode(want)
		}))
		defer host.Close()
		state := filepath.Join(t.TempDir(), "absent-state")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stdout, stderr, err := invokeTaskCLIProcessWithEnvAt(ctx, "", map[string]string{
			"AMUX_GATEWAY_KEY":       "fixture-key",
			"AMUX_STATE_DIR":         state,
			"AMUX_AGENT_SESSION_ID":  "",
			"AMUX_AGENT_INSTANCE_ID": "",
			"AMUX_AGENT_PROVIDER":    "",
			"AMUX_AGENT_OS":          "",
		}, []string{"tasks", "--project", "demo", "--gateway-url", host.URL, "--json"}, "cancellation", taskTestID)
		if err != nil {
			t.Fatalf("actorless read: %v stderr=%q", err, stderr)
		}
		var got storage.Cancellation
		if err := json.Unmarshal(stdout, &got); err != nil || got != want {
			t.Fatalf("read result=%s parsed=%+v err=%v", stdout, got, err)
		}
		if requests.Load() != 1 {
			t.Fatalf("request count=%d, want one", requests.Load())
		}
		if _, statErr := os.Stat(state); !os.IsNotExist(statErr) {
			t.Fatalf("read created state: %v", statErr)
		}
	})
}

func TestTasksRejectsConflictingGatewayURLFlagsAndHonorsExplicitKeyEnv(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer host.Close()
	var stdout, stderr bytes.Buffer
	err := runTasks(context.Background(), []string{"--server", host.URL, "--gateway-url", "http://127.0.0.1:1", "--project", "demo", "cancellation", taskTestID}, &stdout, &stderr, func(string) string { return "fixture-key" })
	if err == nil || !strings.Contains(err.Error(), "conflict") || requests.Load() != 0 {
		t.Fatalf("conflict err=%v requests=%d", err, requests.Load())
	}
	stdout.Reset()
	stderr.Reset()
	err = runTasks(context.Background(), []string{"--server", host.URL, "--project", "demo", "--api-key-env", "CUSTOM_KEY", "cancellation", taskTestID}, &stdout, &stderr, func(name string) string {
		if name == "AMUX_GATEWAY_KEY" {
			return "fallback-must-not-be-used"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "credential is not configured") || requests.Load() != 0 {
		t.Fatalf("explicit key env err=%v requests=%d", err, requests.Load())
	}
}
