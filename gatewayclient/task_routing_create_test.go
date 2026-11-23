package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestCreateRoutedTaskModesAndAcknowledgements(t *testing.T) {
	actor := delegationTestActor()
	target := storage.RegisteredTargetIdentity{Origin: actor, Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}
	for _, mode := range []string{TaskRoutingAuto, TaskRoutingUnassigned, TaskRoutingBroadcast, TaskRoutingSession} {
		t.Run(mode, func(t *testing.T) {
			id := uuid.NewString()
			remote := "github.com/example/project"
			archivedAt := int64(1)
			var requests int
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method == http.MethodGet && r.URL.Path == "/v1/projects" {
					unrelatedRemoteA := "gitlab.com/other/repo"
					unrelatedRemoteB := "https://gitlab.com/other/repo.git"
					_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{
						{Project: storage.Project{Ident: "demo", CanonicalRemote: &remote}},
						{Project: storage.Project{Ident: "archived", ArchivedAt: &archivedAt, CanonicalRemote: &remote}},
						{Project: storage.Project{Ident: "other-a", CanonicalRemote: &unrelatedRemoteA}},
						{Project: storage.Project{Ident: "other-b", CanonicalRemote: &unrelatedRemoteB}},
					})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/tasks" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", http.StatusBadRequest)
					return
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["canonical_remote"]) != `"github.com/example/project"` {
					t.Errorf("invalid routed create body: %+v", body)
				}
				if mode == TaskRoutingAuto {
					if _, exists := body["routing"]; exists {
						t.Error("auto route should use the server default")
					}
				} else {
					var sent storage.TaskRouting
					if json.Unmarshal(body["routing"], &sent) != nil || sent.Mode != mode {
						t.Errorf("routing request = %s", body["routing"])
					}
					if mode == TaskRoutingSession && (sent.Target == nil || *sent.Target != target) {
						t.Errorf("session target changed: %+v", sent.Target)
					}
				}
				owner := any(nil)
				if mode == TaskRoutingSession {
					owner = target.Origin
				}
				ackRouting := storage.TaskRouting{Mode: mode}
				if mode == TaskRoutingSession {
					ackRouting.Target = &target
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": id, "project_ident": "demo", "status": "todo", "parent": nil,
					"routing": ackRouting, "owner_origin": owner,
				})
			}))
			defer host.Close()
			client, err := New(host.URL, "fixture-key", host.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.CreateRoutedTask(context.Background(), "demo", TaskCreateInput{Title: "Routed"}, &remote, storage.TaskRouting{Mode: mode, Target: targetForMode(mode, target)})
			if err != nil || result == nil || result.ID != id {
				t.Fatalf("create result=%+v err=%v", result, err)
			}
			if requests != 2 {
				t.Fatalf("requests=%d, want one project check and one create", requests)
			}
		})
	}
}

func TestCreateRoutedTaskRejectsAmbiguousActiveProjectInventoryBeforePost(t *testing.T) {
	requestedRemote := "github.com/example/project"
	for _, scenario := range []string{"duplicate identity", "duplicate normalized repository"} {
		t.Run(scenario, func(t *testing.T) {
			var posts int
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/projects" {
					posts++
					http.Error(w, "task creation must not be attempted", http.StatusBadRequest)
					return
				}
				projectsList := []storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: &requestedRemote}}}
				if scenario == "duplicate identity" {
					otherRemote := "github.com/example/other"
					projectsList = append(projectsList, storage.ProjectOverview{Project: storage.Project{Ident: "demo", CanonicalRemote: &otherRemote}})
				} else {
					otherIdentRemote := "https://github.com/example/project.git"
					projectsList = append(projectsList, storage.ProjectOverview{Project: storage.Project{Ident: "other", CanonicalRemote: &otherIdentRemote}})
				}
				_ = json.NewEncoder(w).Encode(projectsList)
			}))
			defer host.Close()
			client, _ := New(host.URL, "fixture-key", host.Client())
			_, err := client.CreateRoutedTask(context.Background(), "demo", TaskCreateInput{Title: "Routed"}, &requestedRemote, storage.TaskRouting{Mode: TaskRoutingBroadcast})
			if err == nil || posts != 0 {
				t.Fatalf("ambiguous inventory err=%v task POSTs=%d", err, posts)
			}
		})
	}
}

func TestCreateRoutedTaskRejectsUnconfirmedReceiptAndDoesNotRetry(t *testing.T) {
	var requests int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method == http.MethodGet {
			remote := "github.com/example/project"
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: &remote}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": uuid.NewString(), "project_ident": "demo", "status": "todo", "parent": nil,
			"routing": storage.TaskRouting{Mode: TaskRoutingBroadcast}, "owner_origin": nil,
		})
	}))
	defer host.Close()
	client, _ := New(host.URL, "fixture-key", host.Client())
	remote := "github.com/example/project"
	_, err := client.CreateRoutedTask(context.Background(), "demo", TaskCreateInput{Title: "Routed"}, &remote, storage.TaskRouting{Mode: TaskRoutingUnassigned})
	if err == nil || !strings.Contains(err.Error(), "inspect the task before retrying") {
		t.Fatalf("unconfirmed receipt error=%v", err)
	}
	if requests != 2 {
		t.Fatalf("request count=%d, want one read and one non-retried create", requests)
	}
}

func TestCreateRoutedTaskAllowsExplicitRoutingForAdHocProject(t *testing.T) {
	id := uuid.NewString()
	var requests int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method == http.MethodGet && r.URL.Path == "/v1/projects" {
			unrelatedRemoteA := "gitlab.com/other/repo"
			unrelatedRemoteB := "https://gitlab.com/other/repo.git"
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{
				{Project: storage.Project{Ident: "local", Kind: "adhoc"}},
				{Project: storage.Project{Ident: "other-a", CanonicalRemote: &unrelatedRemoteA}},
				{Project: storage.Project{Ident: "other-b", CanonicalRemote: &unrelatedRemoteB}},
			})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/local/tasks" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["routing"]) != `{"mode":"broadcast"}` {
			t.Errorf("invalid ad hoc routing request: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "project_ident": "local", "status": "todo", "parent": nil,
			"routing": storage.TaskRouting{Mode: TaskRoutingBroadcast}, "owner_origin": nil,
		})
	}))
	defer host.Close()
	client, _ := New(host.URL, "fixture-key", host.Client())
	task, err := client.CreateRoutedTask(context.Background(), "local", TaskCreateInput{Title: "Broadcast"}, nil, storage.TaskRouting{Mode: TaskRoutingBroadcast})
	if err != nil || task == nil || task.ID != id || requests != 2 {
		t.Fatalf("task=%+v requests=%d err=%v", task, requests, err)
	}
}

func TestCreateRoutedTaskValidatesSessionTargetBeforeGatewayMutation(t *testing.T) {
	var requests int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer host.Close()
	client, _ := New(host.URL, "fixture-key", host.Client())
	bad := storage.RegisteredTargetIdentity{Origin: delegationTestActor(), Generation: "not-a-uuid"}
	_, err := client.CreateRoutedTask(context.Background(), "demo", TaskCreateInput{Title: "Routed"}, nil, storage.TaskRouting{Mode: TaskRoutingSession, Target: &bad})
	if err == nil || !strings.Contains(err.Error(), "complete canonical target") || requests != 0 {
		t.Fatalf("validation err=%v requests=%d", err, requests)
	}
}

func targetForMode(mode string, target storage.RegisteredTargetIdentity) *storage.RegisteredTargetIdentity {
	if mode == TaskRoutingSession {
		return &target
	}
	return nil
}
