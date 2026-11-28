package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func requesterClientFixture(t *testing.T) (*storage.Store, http.Handler, *storage.Task, agent.Origin) {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "requesters.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, project := range []string{"source", "target"} {
		if err := s.InsertProject(ctx, storage.Project{Ident: project, ChannelName: project, Kind: "adhoc"}); err != nil {
			t.Fatal(err)
		}
	}
	actor := delegationTestActor()
	parent, err := s.InsertTask(ctx, storage.TaskInsert{Project: "source", Title: "Parent", Reporter: actor.SessionID}, &actor)
	if err != nil {
		t.Fatal(err)
	}
	api, err := server.NewHandler(s, "requester-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return s, api, parent, actor
}

func TestRequesterClientRoundTripAndDelegationCapabilityInteroperate(t *testing.T) {
	s, api, parent, actor := requesterClientFixture(t)
	ctx := context.Background()
	host := httptest.NewServer(api)
	defer host.Close()
	client, _ := New(host.URL, "requester-fixture", host.Client())
	registered, err := client.RegisterTarget(ctx, "source", storage.TargetRegistration{Origin: actor, Generation: actor.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	self, err := client.RequesterTarget(ctx, "source", actor)
	if err != nil || self == nil || *self != registered.Target {
		t.Fatalf("self=%+v err=%v", self, err)
	}
	raw, err := client.TaskCreate(ctx, "source", TaskCreateInput{Title: "Requested work", ParentID: &parent.ID, Requester: self}, &actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	var task storage.Task
	if json.Unmarshal(raw, &task) != nil {
		t.Fatalf("invalid created task %s", raw)
	}
	watcher := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	watcherRegistration, err := client.RegisterTarget(ctx, "source", storage.TargetRegistration{Origin: watcher, Generation: watcher.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.SetTaskRequester(ctx, "source", task.ID, &watcherRegistration.Target, nil)
	if err != nil || updated.OwnerOrigin == nil || *updated.OwnerOrigin != actor || updated.Status != "todo" {
		t.Fatalf("human selected watcher changed work: %+v %v", updated, err)
	}
	cleared, err := client.SetTaskRequester(ctx, "source", task.ID, nil, &watcher)
	if err != nil || string(cleared.Requester) != "null" || cleared.OwnerOrigin == nil || *cleared.OwnerOrigin != actor {
		t.Fatalf("requester could not clear independently: %+v %v", cleared, err)
	}
	input := TaskDelegateInput{TargetProject: "target", Title: "Cross-project handoff", ParentID: &parent.ID, Requester: self}
	delegated, err := client.DelegateTask(ctx, "source", input, &actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledged storage.RegisteredTargetIdentity
	if json.Unmarshal(delegated.SourceTask.Requester, &acknowledged) != nil || acknowledged != *self || string(delegated.TargetTask.Requester) != "null" || delegated.TargetTask.OwnerOrigin != nil {
		t.Fatalf("delegation requester/executor scopes changed: %+v", delegated)
	}
	if _, err := client.TaskUpdate(ctx, "target", delegated.TargetTask.ID, storage.TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage("null")}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.TaskUpdate(ctx, "source", delegated.SourceTask.ID, storage.TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage("null")}, &actor, nil); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Task(ctx, "target", delegated.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := false
	for _, comment := range detail.Comments {
		confirmed = confirmed || strings.Contains(comment.Content, "confirmed completion")
	}
	if !confirmed {
		t.Fatal("source acceptance did not confirm target completion")
	}
}

func TestRequesterSetterDoesNotReplayAfterCommittedConnectionLoss(t *testing.T) {
	s, api, parent, actor := requesterClientFixture(t)
	ctx := context.Background()
	var puts atomic.Int32
	var capabilityRemote, mutationRemote atomic.Value
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/tasks/capabilities" {
			capabilityRemote.Store(r.RemoteAddr)
		}
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/requester") {
			api.ServeHTTP(w, r)
			return
		}
		puts.Add(1)
		mutationRemote.Store(r.RemoteAddr)
		result := httptest.NewRecorder()
		api.ServeHTTP(result, r)
		if result.Code != http.StatusOK {
			w.WriteHeader(result.Code)
			w.Write(result.Body.Bytes())
			return
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack committed requester response: %v", err)
			return
		}
		connection.Close()
	}))
	defer host.Close()
	client, _ := New(host.URL, "requester-fixture", host.Client())
	registered, err := client.RegisterTarget(ctx, "source", storage.TargetRegistration{Origin: actor, Generation: actor.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SetTaskRequester(ctx, "source", parent.ID, &registered.Target, &actor)
	if result != nil || !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("lost acknowledgement=%+v %v", result, err)
	}
	if puts.Load() != 1 {
		t.Fatalf("committed requester setter replayed %d times", puts.Load())
	}
	if capabilityRemote.Load() == nil || capabilityRemote.Load() != mutationRemote.Load() {
		t.Fatal("fixture did not exercise a reused connection")
	}
	detail, err := s.Task(ctx, "source", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted storage.RegisteredTargetIdentity
	if json.Unmarshal(detail.Task.Requester, &persisted) != nil || persisted != registered.Target {
		t.Fatal("requester mutation did not commit before acknowledgement loss")
	}
}
