package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nitecon/amux/identity/agent"
)

func TestCallTaskToolWithGoalUsesPerRequestHeader(t *testing.T) {
	var observed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/tasks/conversations/conv/tools" {
			t.Errorf("path=%q", r.URL.Path)
		}
		observed = append(observed, r.Header.Values("X-Task-Goal")...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	goal := "  release  "
	if _, err := client.CallTaskToolWithGoal(context.Background(), "tasks", "conv", agent.Origin{}, &goal, "task_list", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTaskTool(context.Background(), "tasks", "conv", agent.Origin{}, "task_list", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if _, err := client.CallTaskToolWithGoal(context.Background(), "tasks", "conv", agent.Origin{}, &empty, "task_list", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] != "release" || observed[1] != "" {
		t.Fatalf("selected-goal request headers=%q, want normalized scope and explicit unscoped scope", observed)
	}
}

func TestCallTaskToolWithGoalDoesNotMutateSharedClient(t *testing.T) {
	var observed [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = append(observed, r.Header.Values("X-Task-Goal"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, second := "alpha", "beta"
	if _, err := client.CallTaskToolWithGoal(context.Background(), "tasks", "conv", agent.Origin{}, &first, "task_list", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTaskToolWithGoal(context.Background(), "tasks", "conv", agent.Origin{}, &second, "task_list", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || len(observed[0]) != 1 || observed[0][0] != first || len(observed[1]) != 1 || observed[1][0] != second {
		t.Fatalf("request scopes=%q, want %q then %q", observed, first, second)
	}
}
