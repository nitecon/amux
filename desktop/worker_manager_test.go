package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func workerManagerFixture(t *testing.T, open worker.Open) (*WorkerManager, *storage.Store, *storage.Store, *gatewayclient.Client, string) {
	t.Helper()
	ctx := context.Background()
	local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	if err := remote.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "manager-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	t.Cleanup(host.Close)
	client, err := gatewayclient.New(host.URL, "manager-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewWorkerManager(WorkerManagerConfig{Store: local, Client: client, Open: open})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, local, remote, client, host.URL
}

func managerPoolConfig(serverURL, directory string) storage.WorkerPool {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	goal := "release"
	return storage.WorkerPool{
		Server: serverURL, Project: "demo", Directory: directory, Provider: "codex",
		OS: osName, SlotCount: 1, Goal: &goal,
	}
}

func TestWorkerManagerCreatesTasklessConversationBeforeOpeningProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var created storage.Conversation
	var opened storage.WorkerRun
	stop := errors.New("synthetic provider stopped")
	manager, _, _, client, _ := workerManagerFixture(t, func(_ context.Context, run storage.WorkerRun, _ conversation.Sink) (worker.Session, error) {
		opened = run
		return nil, stop
	})
	manager.conversationRoot = t.TempDir()
	err := manager.RunConversation(ctx, "demo", "", "antigravity", func(chat storage.Conversation) { created = chat })
	if !errors.Is(err, stop) || created.ID == "" || opened.ConversationID != created.ID || opened.Owner.Provider != "antigravity" {
		t.Fatalf("launch order or result: created=%+v opened=%+v err=%v", created, opened, err)
	}
	wantDirectory, err := filepath.EvalSymlinks(filepath.Join(manager.conversationRoot, "demo", "conversations"))
	if err != nil || opened.Directory != wantDirectory {
		t.Fatalf("conversation working directory = %q, want %q (%v)", opened.Directory, wantDirectory, err)
	}
	reopened, err := client.Conversation(ctx, "demo", created.ID)
	if err != nil || reopened == nil || reopened.ID != created.ID || reopened.TaskID != "" {
		t.Fatalf("reopened taskless conversation=%+v err=%v", reopened, err)
	}
}

func TestWorkerManagerPersistsConversationLaunchPreferencesInRunCommand(t *testing.T) {
	ctx := context.Background()
	var opened storage.WorkerRun
	stop := errors.New("fixture provider stopped")
	manager, local, _, _, _ := workerManagerFixture(t, func(_ context.Context, run storage.WorkerRun, _ conversation.Sink) (worker.Session, error) {
		opened = run
		return nil, stop
	})
	manager.conversationRoot = t.TempDir()
	command, err := providerLaunchCommand("claude", ConversationLaunchPreferences{Model: "sonnet", Persona: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.RunConversationWithCommand(ctx, "demo", "", "claude", command, func(storage.Conversation) {})
	if !errors.Is(err, stop) {
		t.Fatalf("conversation result error=%v", err)
	}
	want := []string{"claude", "--model", "sonnet", "--agent", "reviewer"}
	if !reflect.DeepEqual(opened.Command, want) {
		t.Fatalf("provider launch args=%v want=%v", opened.Command, want)
	}
	saved, err := local.WorkerRun(ctx, opened.ID)
	if err != nil || !reflect.DeepEqual(saved.Command, want) {
		t.Fatalf("saved provider launch args=%v err=%v", saved.Command, err)
	}
}

func TestWorkerManagerBindsRepositoryConversationToCheckoutRoot(t *testing.T) {
	ctx := context.Background()
	var opened storage.WorkerRun
	stop := errors.New("fixture provider stopped")
	manager, _, remote, _, _ := workerManagerFixture(t, func(_ context.Context, run storage.WorkerRun, _ conversation.Sink) (worker.Session, error) {
		opened = run
		return nil, stop
	})
	manager.conversationRoot = t.TempDir()
	upstream := "github.com/example/widget"
	if err := remote.InsertProject(ctx, storage.Project{Ident: "widget", ChannelName: "widget", Kind: "repo", CanonicalRemote: &upstream}); err != nil {
		t.Fatal(err)
	}
	onCreated := func(chat storage.Conversation) {
		if chat.ProjectIdent != "widget" {
			t.Fatalf("created a conversation in %q", chat.ProjectIdent)
		}
	}
	for _, directory := range []string{"", t.TempDir()} {
		if err := manager.RunConversation(ctx, "widget", directory, "codex", onCreated); err == nil || errors.Is(err, stop) {
			t.Fatalf("unbound directory %q reached provider: %v", directory, err)
		}
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet"}, {"remote", "add", "origin", "git@github.com:example/widget.git"}} {
		output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("prepare checkout: %s: %v", output, err)
		}
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RunConversation(ctx, "widget", nested, "codex", onCreated); !errors.Is(err, stop) || opened.Directory != want {
		t.Fatalf("provider directory %q, want %q, error %v", opened.Directory, want, err)
	}
}

func TestWorkerManagerLiveACPTasklessReply(t *testing.T) {
	if os.Getenv("AMUX_LIVE_ACP") != "1" {
		t.Skip("set AMUX_LIVE_ACP=1 to use the installed, authenticated agy ACP executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager, _, remote, client, endpoint := workerManagerFixture(t, worker.OpenACP)
	if executable := os.Getenv("AMUX_LIVE_ACP_TASK_TOOLS"); executable != "" {
		const credentialEnv = "AMUX_LIVE_ACP_GATEWAY_KEY"
		t.Setenv(credentialEnv, "manager-secret")
		manager.open = worker.OpenWithNativeTools(worker.NativeToolsConfig{
			Executable: executable, Server: endpoint, APIKeyEnv: credentialEnv,
			WorkPreferencesPath: filepath.Join(t.TempDir(), "work-preferences.json"),
		})
	}
	created := make(chan storage.Conversation, 1)
	done := make(chan error, 1)
	manager.conversationRoot = t.TempDir()
	go func() {
		done <- manager.RunConversation(ctx, "demo", "", "antigravity", func(chat storage.Conversation) { created <- chat })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("live ACP worker did not exit")
		}
	})
	var chat storage.Conversation
	select {
	case chat = <-created:
	case err := <-done:
		done <- err
		t.Fatalf("create live conversation: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	const marker = "AMUX_ACP_OK"
	if err := client.Enqueue(ctx, chat.ProjectIdent, chat.ID, uuid.NewString(), conversation.Command{
		Kind: "prompt", Text: "Reply with only " + marker + ". Do not use tools.",
	}); err != nil {
		t.Fatal(err)
	}
	for {
		changed := remote.ConversationChanges()
		events, err := remote.ConversationEvents(ctx, chat.ProjectIdent, chat.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var reply strings.Builder
		completed := false
		for _, record := range events {
			switch record.Event.Kind {
			case "text_delta":
				reply.WriteString(record.Event.Text)
			case "turn_completed":
				completed = record.Event.Status == "end_turn"
			}
		}
		if completed {
			if !strings.Contains(reply.String(), marker) {
				t.Fatal("the live ACP agent completed without the expected reply marker")
			}
			reopened, err := client.Conversation(ctx, chat.ProjectIdent, chat.ID)
			if err != nil || reopened.TaskID != "" || reopened.NativeSessionID == "" {
				t.Fatalf("reopen taskless live conversation: %+v %v", reopened, err)
			}
			return
		}
		select {
		case <-changed:
		case err := <-done:
			done <- err
			t.Fatalf("live ACP worker exited before an agent reply: %v", err)
		case <-ctx.Done():
			t.Fatalf("live ACP reply did not reach the gateway: %v", ctx.Err())
		}
	}
}

func TestBrowserNewConversationKeepsWorkerForWindowLifetime(t *testing.T) {
	app := fynetest.NewApp()
	t.Cleanup(app.Quit)
	deadlines := make(chan bool, 1)
	manager, _, _, client, _ := workerManagerFixture(t, func(ctx context.Context, _ storage.WorkerRun, _ conversation.Sink) (worker.Session, error) {
		_, bounded := ctx.Deadline()
		deadlines <- bounded
		<-ctx.Done()
		return nil, ctx.Err()
	})
	manager.conversationRoot = t.TempDir()
	callbacks := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, WorkerManager: manager, Project: "demo", Dispatch: callbacks.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { browser.Close(); browser.Wait(); callbacks.pump() })
	browser.Start(context.Background())
	waitBrowser(t, callbacks, func() bool { return browser.selectedProject == "demo" })
	browser.setMainView("Conversations")
	waitBrowser(t, callbacks, func() bool { return !browser.projectConversationsLoading })
	browser.projectConversationList.Select(2)
	select {
	case bounded := <-deadlines:
		if bounded {
			t.Fatal("native provider inherited the short gateway request timeout")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("New Conversation did not start the worker")
	}
}

func waitForManagerPresence(t *testing.T, ctx context.Context, manager *WorkerManager, remote *storage.Store) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		agents, err := remote.Presence(ctx, "demo")
		if err != nil {
			t.Fatal(err)
		}
		if len(agents) > 0 {
			return
		}
		snapshot := manager.Snapshot()
		if snapshot.State == WorkerManagerFailed {
			t.Fatalf("worker manager failed before presence: %+v", snapshot)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker manager never advertised presence: %+v", manager.Snapshot())
		case <-ticker.C:
		}
	}
}

func TestWorkerManagerCreateResumeStopAndSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var opens atomic.Int32
	manager, local, remote, _, endpoint := workerManagerFixture(t, func(context.Context, storage.WorkerRun, conversation.Sink) (worker.Session, error) {
		opens.Add(1)
		return nil, nil
	})
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	requested := managerPoolConfig(endpoint, t.TempDir())
	created, err := manager.CreateAndStartPool(requested)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Server != endpoint || created.Directory != requested.Directory || created.Project != requested.Project {
		t.Fatalf("created pool did not preserve its immutable configuration: %+v", created)
	}
	if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerRunning || snapshot.Pool == nil || snapshot.Pool.ID != created.ID || snapshot.Generation != 1 {
		t.Fatalf("running snapshot: %+v", snapshot)
	}
	wire, _ := json.Marshal(created)
	if strings.Contains(string(wire), "manager-secret") {
		t.Fatal("gateway credential appeared in persisted pool representation")
	}
	created.Project = "mutated-return-value"
	view := manager.Snapshot()
	view.Pool.Project = "mutated-snapshot"
	if snapshot := manager.Snapshot(); snapshot.Pool.Project != "demo" {
		t.Fatalf("snapshot exposed mutable pool state: %+v", snapshot)
	}
	waitForManagerPresence(t, ctx, manager, remote)
	manager.Stop()
	if err := manager.Wait(); err != nil {
		t.Fatal(err)
	}
	stopped := manager.Snapshot()
	if stopped.State != WorkerManagerStopped || stopped.Pool == nil || stopped.Pool.ID != created.ID {
		t.Fatalf("stopped snapshot lost saved pool: %+v", stopped)
	}
	saved, err := local.WorkerPool(ctx, created.ID)
	if err != nil || saved.Server != endpoint || saved.Project != "demo" || saved.Directory != requested.Directory || saved.Goal == nil || *saved.Goal != "release" {
		t.Fatalf("saved pool changed: %+v %v", saved, err)
	}

	firstGeneration := stopped.Generation
	if err := manager.ResumePool(saved.ID); err != nil {
		t.Fatal(err)
	}
	if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerRunning || snapshot.Generation != firstGeneration+1 || snapshot.Pool.ID != saved.ID || snapshot.Pool.Directory != saved.Directory || snapshot.Pool.Goal == nil || *snapshot.Pool.Goal != "release" {
		t.Fatalf("resumed snapshot changed saved configuration: %+v", snapshot)
	}
	waitForManagerPresence(t, ctx, manager, remote)
	manager.Stop()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 {
		t.Fatalf("manager launched %d provider sessions without assignments", opens.Load())
	}
}

func TestWorkerManagerSnapshotsErrorsAndAllowsOnlyOneActivePool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager, local, remote, _, endpoint := workerManagerFixture(t, nil)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bad := managerPoolConfig("https://other.example", t.TempDir())
	failedPool, err := manager.CreateAndStartPool(bad)
	if err == nil || failedPool != nil {
		t.Fatalf("accepted pool for another server: %+v %v", failedPool, err)
	}
	failed := manager.Snapshot()
	if failed.State != WorkerManagerFailed || failed.Error == "" || failed.Pool == nil || failed.Pool.ID == "" || failed.Pool.Server != "https://other.example" {
		t.Fatalf("failed create lost its request snapshot: %+v", failed)
	}
	if _, err := local.WorkerPool(ctx, failed.Pool.ID); err == nil {
		t.Fatal("invalid server pool was persisted")
	}

	if err := manager.ResumePool("missing-saved-pool"); err == nil {
		t.Fatal("resumed a missing saved pool")
	}
	failed = manager.Snapshot()
	if failed.State != WorkerManagerFailed || failed.Pool == nil || failed.Pool.ID != "missing-saved-pool" || failed.Error == "" {
		t.Fatalf("failed resume snapshot lost requested ID: %+v", failed)
	}

	pool, err := manager.CreateAndStartPool(managerPoolConfig(endpoint, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	waitForManagerPresence(t, ctx, manager, remote)
	if _, err := manager.CreateAndStartPool(managerPoolConfig(endpoint, t.TempDir())); err == nil {
		t.Fatal("started a second pool in one manager")
	}
	if snapshot := manager.Snapshot(); snapshot.State != WorkerManagerRunning || snapshot.Pool == nil || snapshot.Pool.ID != pool.ID {
		t.Fatalf("rejected second start damaged active snapshot: %+v", snapshot)
	}
	manager.Stop()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.ResumePool(pool.ID); err == nil {
		t.Fatal("restarted a closed manager")
	}
}

func TestWorkerManagerPreservesNativeDeadlineFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	manager, _, remote, _, endpoint := workerManagerFixture(t, func(context.Context, storage.WorkerRun, conversation.Sink) (worker.Session, error) {
		return nil, context.DeadlineExceeded
	})
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Synthetic provider failure", Reporter: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAndStartPool(managerPoolConfig(endpoint, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	waitForManagerPresence(t, ctx, manager, remote)
	if err := remote.ReconcileAssignments(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for manager.Snapshot().State != WorkerManagerFailed {
		select {
		case <-ctx.Done():
			t.Fatalf("synthetic provider deadline was not visible: %+v", manager.Snapshot())
		case <-ticker.C:
		}
	}
	snapshot := manager.Snapshot()
	if !strings.Contains(snapshot.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("provider deadline was normalized as clean stop: %+v", snapshot)
	}
	if err := manager.Wait(); err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Wait lost provider deadline: %v", err)
	}
	if err := manager.Close(); err == nil {
		t.Fatal("Close lost provider failure")
	}
}

func TestWorkerManagerStartupValidationFailureKeepsPersistedPoolID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager, local, _, _, endpoint := workerManagerFixture(t, nil)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	requested := managerPoolConfig(endpoint, t.TempDir())
	requested.ID = uuid.NewString()
	persisted, err := local.CreateWorkerPool(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	gen, runCtx, _, err := manager.reserve(persisted)
	if err != nil {
		t.Fatal(err)
	}
	manager.failStart(gen, runCtx, errors.New("simulated read-after-insert validation failure"))
	snapshot := manager.Snapshot()
	if snapshot.State != WorkerManagerFailed || snapshot.Error == "" || snapshot.Pool == nil || snapshot.Pool.ID != persisted.ID {
		t.Fatalf("post-insert failure lost saved pool identity: %+v", snapshot)
	}
}

func TestWorkerManagerKeepsStandaloneCanceledProviderFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	manager, _, remote, _, endpoint := workerManagerFixture(t, func(context.Context, storage.WorkerRun, conversation.Sink) (worker.Session, error) {
		return nil, context.Canceled
	})
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Synthetic provider cancellation", Reporter: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAndStartPool(managerPoolConfig(endpoint, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	waitForManagerPresence(t, ctx, manager, remote)
	if err := remote.ReconcileAssignments(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for manager.Snapshot().State != WorkerManagerFailed {
		select {
		case <-ctx.Done():
			t.Fatalf("synthetic provider cancellation was not visible: %+v", manager.Snapshot())
		case <-ticker.C:
		}
	}
	if !strings.Contains(manager.Snapshot().Error, context.Canceled.Error()) {
		t.Fatalf("provider cancellation was normalized as a clean stop: %+v", manager.Snapshot())
	}
}
