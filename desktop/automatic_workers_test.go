package desktop

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

type automaticFixtureSession struct{ *markdownFixtureSession }

func (s *automaticFixtureSession) Prompt(ctx context.Context, text string) (string, error) {
	result, err := s.markdownFixtureSession.Prompt(ctx, text)
	if err == nil {
		err = s.sink(ctx, conversation.Event{Kind: "turn_completed", SessionID: "fixture-native"})
	}
	return result, err
}

func TestAutomaticWorkerRegistersAndArchivesAssignedTask(t *testing.T) {
	t.Run("native gateway", func(t *testing.T) { testAutomaticAssignment(t, true) })
	t.Run("Rust-compatible gateway", func(t *testing.T) { testAutomaticAssignment(t, false) })
}

func testAutomaticAssignment(t *testing.T, native bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var streams, adoptionRequests, opens atomic.Int32
	manager, local, remote, client, _ := workerManagerFixture(t, func(ctx context.Context, run storage.WorkerRun, sink conversation.Sink) (worker.Session, error) {
		opens.Add(1)
		if run.Owner.Provider != "codex" {
			t.Errorf("opened unexpected provider %q", run.Owner.Provider)
		}
		if err := sink(ctx, conversation.Event{Kind: "session_ready", SessionID: "fixture-native"}); err != nil {
			return nil, err
		}
		return &automaticFixtureSession{&markdownFixtureSession{sink: sink, done: make(chan struct{})}}, nil
	}, func(w http.ResponseWriter, r *http.Request) bool {
		if !native {
			if r.URL.Path == "/v1/tasks/capabilities" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
				return true
			}
			if strings.HasSuffix(r.URL.Path, "/worker-assignment") || r.URL.Path == "/v1/worker-presence" {
				t.Errorf("Rust-compatible worker used Go-only endpoint %s", r.URL.Path)
				http.NotFound(w, r)
				return true
			}
		}
		if r.URL.Path == "/v1/tasks/stream" {
			streams.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/worker-assignment") {
			adoptionRequests.Add(1)
		}
		return false
	})
	root, checkout := t.TempDir(), t.TempDir()
	if output, err := exec.Command("git", "-C", checkout, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture checkout: %v: %s", err, output)
	}
	manager.archive = archiveTaskWorker(root, client)
	manager.taskConversationRoot = root
	manager.useTaskStream = true
	streamCtx, disconnect := context.WithCancel(ctx)
	defer disconnect()
	if err := manager.Start(streamCtx); err != nil {
		t.Fatal(err)
	}
	updates := make(chan []automaticWorkerScope, 1)
	updates <- []automaticWorkerScope{{storage.ProjectOverview{Project: storage.Project{Ident: "demo", Kind: "adhoc"}}, checkout}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.runAutomaticWorkers(ctx, updates, func(string) {}, func(_ context.Context, provider, _ string) bool {
			return provider == "codex" || provider == "antigravity"
		})
	}()
	defer func() { cancel(); <-done }()
	var target storage.RegisteredTargetIdentity
	waitFor := func(condition func() bool) {
		t.Helper()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for !condition() {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("timed out waiting for automatic agent workflow")
			}
		}
	}
	waitFor(func() bool {
		targets, err := client.Targets(ctx, "demo")
		if err != nil || len(targets) != 2 {
			return false
		}
		for _, registered := range targets {
			name := worker.AgentDisplayName(registered.Target.Origin)
			if registered.DisplayName == nil || *registered.DisplayName != name || !strings.Contains(name, " - ") {
				t.Fatalf("agent registration omitted identity name: %+v", registered.DisplayName)
			}
			if registered.Target.Origin.Provider == "codex" {
				target = registered.Target
			}
			if registered.Target.Origin.Provider == "claude" {
				t.Fatal("unavailable Claude provider was registered")
			}
		}
		return target.Origin.SessionID != ""
	})
	if streams.Load() != 1 {
		t.Fatalf("task stream connections: %d, want 1", streams.Load())
	}
	// Wait beyond the legacy two-second polling interval.
	time.Sleep(2200 * time.Millisecond)
	if got := adoptionRequests.Load(); got != 0 {
		t.Fatalf("idle assignment requests: %d, want 0", got)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Assigned fixture task", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool {
		_, cursor, err := local.WorkerTaskConsumer(ctx, client.Endpoint())
		return err == nil && cursor > 0
	})
	disconnect()
	<-manager.taskStreamDone
	// The gateway releases the consumer lease asynchronously; reconnecting
	// before then is rejected and retried, which would add a connection.
	waitFor(func() bool {
		statuses, err := remote.TaskConsumerStatuses(ctx)
		if err != nil {
			return false
		}
		for _, status := range statuses {
			if status.Connected {
				return false
			}
		}
		return true
	})
	if _, err := remote.AssignTaskFromUser(ctx, "demo", task.ID, target, time.Now()); err != nil {
		t.Fatal(err)
	}
	reconnectDone := make(chan struct{})
	go func() {
		defer close(reconnectDone)
		worker.WatchAssignments(ctx, local, client)
	}()
	defer func() { cancel(); <-reconnectDone }()
	path := filepath.Join(root, "demo", "tasks", task.ID, target.Origin.SessionID+".md")
	waitFor(func() bool {
		content, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(content), "> Local reply") && strings.Contains(string(content), "Assigned fixture task")
	})
	if got := streams.Load(); got != 2 {
		t.Fatalf("task stream connections after reconnect: %d, want 2", got)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("provider opened %d times, want 1", got)
	}
}

func TestAssignmentStreamBootstrapsAlreadyOwnedTasks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager, local, remote, client, _ := workerManagerFixture(t, nil)
	pool, err := manager.automaticPool(ctx, uuid.NewString(), "demo", t.TempDir(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	slots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := slots[0].Owner
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Already assigned", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.TransitionTask(ctx, "demo", task.ID, "in_progress", &owner); err != nil {
		t.Fatal(err)
	}
	manager.useTaskStream = true
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := local.PendingWorkerTask(ctx, client.Endpoint(), "demo", owner)
		if err != nil {
			t.Fatal(err)
		}
		if pending.TaskID == task.ID {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("current assignment was skipped at the stream boundary")
		}
	}
}
