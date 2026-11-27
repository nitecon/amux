package amuxruntime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestServerRouterOwnsCompleteRegistryAndCompletionSources(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "builtin-router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "builtin", ChannelName: "builtin", RoomID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	s.UseRouterReviews()
	builtins, err := server.NewGatewayRouter(s, 30, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"assignment", "inbound", "native-delivery", "notifier", "presence", "project-inbox", "reclaim", "reminder", "remote", "retention", "review"}
	if got := builtins.Registry().PluginIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("incomplete registry: %v", got)
	}
	runCtx, stop := context.WithCancel(ctx)
	done, ready := make(chan error, 1), make(chan struct{})
	go func() { done <- builtins.Run(runCtx, ready) }()
	joined := false
	defer func() {
		stop()
		if !joined {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		joined = true
		t.Fatalf("startup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("router did not become ready")
	}
	// A second controller cannot acquire the store while this runtime owns it.
	if token, err := s.AcquireRouterRuntime(ctx, builtins.Registry(), time.Now().UnixMilli()); err == nil {
		s.ReleaseRouterRuntime(ctx, token)
		t.Fatal("second runtime acquired active journal")
	}
	watch := s.EventBus().Subscribe(128)
	defer watch.Unsubscribe()
	actor := &storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "builtin", Title: "Finish through the full router", Reporter: "fixture"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, task.ProjectIdent, task.ID, storage.TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, actor); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		children, err := s.TaskReviewChildren(ctx, task.ProjectIdent, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) == 2 {
			break
		}
		select {
		case <-watch.C:
		case <-deadline.C:
			t.Fatalf("completion source did not create exactly one review pair: %+v", children)
		}
	}
	// The complete production registry also owns allocation. The completing
	// actor is excluded from its own review children and may take ordinary work.
	observed := time.Now().Add(-31 * time.Second)
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, storage.PresenceReport{Source: "native:builtin-test", Sequence: 1,
		Agents: []storage.PresenceAgent{{ProjectIdent: task.ProjectIdent, Origin: *actor, AutoAssign: true, LastActivityAt: func() *int64 { n := observed.UnixMilli(); return &n }()}},
	}, observed, "api-key"); err != nil || !accepted {
		t.Fatalf("native allocation report: %t %v", accepted, err)
	}
	queued, err := s.InsertTask(ctx, storage.TaskInsert{Project: task.ProjectIdent, Title: "Allocate through production plugins", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var observedTick bool
	for {
		current, err := s.Task(ctx, queued.ProjectIdent, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		if observedTick && current.Task.Status == "todo" && current.Task.OwnerOrigin != nil && *current.Task.OwnerOrigin == *actor {
			break
		}
		select {
		case event := <-watch.C:
			observedTick = observedTick || event.Topic == eventbus.Tick
		case <-deadline.C:
			t.Fatalf("production clock did not allocate ordinary task: %+v", current)
		}
	}
	run, err := s.AdoptWorkerAssignment(ctx, queued.ProjectIdent, actor)
	if err != nil || run == nil || run.Task.Status != "in_progress" || run.Task.StartedAt == nil {
		t.Fatalf("worker did not start the reserved task: %+v %v", run, err)
	}
	stop()
	select {
	case err := <-done:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("router did not join owned sources")
	}
	children, err := s.TaskReviewChildren(ctx, task.ProjectIdent, task.ID)
	if err != nil || len(children) != 2 {
		t.Fatalf("duplicate completion effects: %+v %v", children, err)
	}
	token, err := s.AcquireRouterRuntime(ctx, builtins.Registry(), time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("stopped runtime kept its journal lease: %v", err)
	}
	if err := s.ReleaseRouterRuntime(ctx, token); err != nil {
		t.Fatal(err)
	}
}
