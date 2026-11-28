package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestReassignTaskUsesDirectoryBindingAndExactLiveWireTarget(t *testing.T) {
	actor := agent.Origin{SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "22222222-2222-4222-8222-222222222222", Provider: "codex", OS: "linux"}
	target := storage.RegisteredTargetIdentity{Origin: agent.Origin{SessionID: "33333333-3333-4333-8333-333333333333", InstanceID: "44444444-4444-4444-8444-444444444444", Provider: "claude", OS: "windows"}, Generation: "55555555-5555-4555-8555-555555555555"}
	const taskID = "66666666-6666-4666-8666-666666666666"
	const remote = "github.com/example/project"
	now := time.Now().UnixMilli()
	current := storage.Task{ID: taskID, ProjectIdent: "demo", Status: "in_progress", Labels: []string{"os:windows"}, OwnerOrigin: &actor}
	assigned := storage.Task{ID: taskID, ProjectIdent: "demo", Status: "todo", Labels: []string{"os:windows"}, OwnerOrigin: &target.Origin, Routing: json.RawMessage(`{"mode":"session","target":{"origin":{"session_id":"33333333-3333-4333-8333-333333333333","instance_id":"44444444-4444-4444-8444-444444444444","provider":"claude","os":"windows"},"generation":"55555555-5555-4555-8555-555555555555"}}`)}
	var posts atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: ptrString(remote)}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/task-targets":
			// Real server wire shape: repository binding is directory-level;
			// individual registration rows contain only target and lease data.
			_ = json.NewEncoder(w).Encode(storage.TargetDirectory{CanonicalRemote: ptrString(remote), Targets: []storage.RegisteredTarget{{Target: target, SeenAt: now, ExpiresAt: now + 120000}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/demo/tasks/"+taskID+"/reassign":
			attempt := posts.Add(1)
			if got := r.Header.Get("X-Agent-Session-Id"); got != actor.SessionID {
				t.Errorf("actor header = %q", got)
			}
			var body struct {
				Target storage.RegisteredTargetIdentity `json:"target"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Target != target {
				t.Errorf("request target=%+v err=%v", body.Target, err)
			}
			receipt := assigned
			if attempt > 1 {
				receipt.Routing = json.RawMessage(strings.Replace(string(assigned.Routing), target.Generation, "77777777-7777-4777-8777-777777777777", 1))
			}
			_ = json.NewEncoder(w).Encode(receipt)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	client, err := New(host.URL, "key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ReassignTask(context.Background(), "demo", taskID, target.Origin.SessionID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || got.OwnerOrigin == nil || *got.OwnerOrigin != target.Origin {
		t.Fatalf("receipt=%+v posts=%d", got, posts.Load())
	}
	if _, err := client.ReassignTask(context.Background(), "demo", taskID, target.Origin.SessionID, actor); err == nil || !strings.Contains(err.Error(), "did not confirm exact target routing") {
		t.Fatalf("stale routing receipt error=%v", err)
	}
	if posts.Load() != 2 {
		t.Fatalf("reassignment retried the stale receipt: posts=%d", posts.Load())
	}
}

func TestReassignTaskRejectsWrongOSBeforeMutation(t *testing.T) {
	actor := agent.Origin{SessionID: "11111111-1111-4111-8111-111111111111", InstanceID: "22222222-2222-4222-8222-222222222222", Provider: "codex", OS: "linux"}
	const taskID = "66666666-6666-4666-8666-666666666666"
	const session = "33333333-3333-4333-8333-333333333333"
	now := time.Now().UnixMilli()
	current := storage.Task{ID: taskID, ProjectIdent: "demo", Status: "todo", Labels: []string{"os:windows"}, OwnerOrigin: &actor}
	var writes int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+taskID):
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: ptrString("github.com/example/project")}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/task-targets"):
			_ = json.NewEncoder(w).Encode(storage.TargetDirectory{CanonicalRemote: ptrString("github.com/example/project"), Targets: []storage.RegisteredTarget{{Target: storage.RegisteredTargetIdentity{Origin: agent.Origin{SessionID: session, InstanceID: "44444444-4444-4444-8444-444444444444", Provider: "claude", OS: "linux"}, Generation: "55555555-5555-4555-8555-555555555555"}, SeenAt: now, ExpiresAt: now + 120000}}})
		default:
			writes++
		}
	}))
	defer host.Close()
	client, _ := New(host.URL, "key", host.Client())
	if _, err := client.ReassignTask(context.Background(), "demo", taskID, session, actor); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error=%v; want incompatible recipient", err)
	}
	if writes != 0 {
		t.Fatalf("mutation requests=%d", writes)
	}
}

func TestAssignTaskUsesSelectedLiveTargetWithoutAgentIdentity(t *testing.T) {
	const taskID = "66666666-6666-4666-8666-666666666666"
	const remote = "github.com/example/project"
	target := storage.RegisteredTargetIdentity{Origin: agent.Origin{SessionID: "33333333-3333-4333-8333-333333333333", InstanceID: "44444444-4444-4444-8444-444444444444", Provider: "claude", OS: "windows"}, Generation: "55555555-5555-4555-8555-555555555555"}
	now := time.Now().UnixMilli()
	current := storage.Task{ID: taskID, ProjectIdent: "demo", Status: "in_progress", Labels: []string{"os:windows"}}
	assigned := storage.Task{ID: taskID, ProjectIdent: "demo", Status: "todo", Labels: []string{"os:windows"}, OwnerOrigin: &target.Origin, Routing: json.RawMessage(`{"mode":"session","target":{"origin":{"session_id":"33333333-3333-4333-8333-333333333333","instance_id":"44444444-4444-4444-8444-444444444444","provider":"claude","os":"windows"},"generation":"55555555-5555-4555-8555-555555555555"}}`)}
	var posted atomic.Bool
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			_ = json.NewEncoder(w).Encode([]storage.ProjectOverview{{Project: storage.Project{Ident: "demo", CanonicalRemote: ptrString(remote)}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo/task-targets":
			_ = json.NewEncoder(w).Encode(storage.TargetDirectory{CanonicalRemote: ptrString(remote), Targets: []storage.RegisteredTarget{{Target: target, SeenAt: now, ExpiresAt: now + 120000}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects/demo/tasks/"+taskID+"/assign":
			if r.Header.Get("X-Agent-Session-Id") != "" || r.Header.Get("X-Agent-Id") != "" {
				t.Errorf("human assignment sent agent identity headers")
			}
			var body struct {
				Target storage.RegisteredTargetIdentity `json:"target"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Target != target {
				t.Errorf("request target=%+v err=%v", body.Target, err)
			}
			posted.Store(true)
			_ = json.NewEncoder(w).Encode(assigned)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	client, err := New(host.URL, "key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.AssignTask(context.Background(), "demo", taskID, target)
	if err != nil || !posted.Load() || got.OwnerOrigin == nil || *got.OwnerOrigin != target.Origin {
		t.Fatalf("assignment=%+v posted=%v err=%v", got, posted.Load(), err)
	}
}
