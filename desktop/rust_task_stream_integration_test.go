package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func TestRustGatewayTaskStreamOverMTLS(t *testing.T) {
	binary := os.Getenv("AMUX_RUST_TEST_BINARY")
	if binary == "" {
		t.Skip("set AMUX_RUST_TEST_BINARY to the Rust amux executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	profile := t.TempDir()
	db, err := storage.Open(ctx, filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	remote := "https://github.com/amux-fixture/demo"
	project := storage.Project{Ident: "demo", ChannelName: "demo", Kind: "repo", CanonicalRemote: &remote}
	if err := db.InsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	listen := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().String()
	}
	api, discovery := listen(), listen()
	cmd := exec.CommandContext(ctx, binary, "server", "secure", "--profile", profile, "--bind", api, "--discovery-bind", discovery, "--name", "127.0.0.1", "--instance-id", uuid.NewString())
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	wait := func(check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal("timed out waiting for Rust gateway workflow")
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	var key string
	wait(func() bool {
		out, err := exec.CommandContext(ctx, binary, "server", "peering-key", "--profile", profile).Output()
		if err == nil {
			key = strings.TrimSpace(string(out))
		}
		return key != ""
	})
	peer, err := storage.PairRemote(ctx, t.TempDir(), "https://"+api, key)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gatewayclient.NewEnrolledFromManifest(peer.Profile.URL, peer.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "local.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	checkout, root := t.TempDir(), t.TempDir()
	if err := exec.CommandContext(ctx, "git", "-C", checkout, "init", "--quiet").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.CommandContext(ctx, "git", "-C", checkout, "remote", "add", "origin", remote).Run(); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	manager, err := NewWorkerManager(WorkerManagerConfig{
		Store: local, Client: client, TaskConversationRoot: root, UseTaskStream: true,
		Open: func(_ context.Context, _ storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
			opens.Add(1)
			return &markdownFixtureSession{sink: sink, done: make(chan struct{})}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	streamCtx, disconnect := context.WithCancel(ctx)
	defer disconnect()
	if err := manager.Start(streamCtx); err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	scopes := make(chan []automaticWorkerScope, 1)
	scopes <- []automaticWorkerScope{{storage.ProjectOverview{Project: project}, checkout}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		manager.runAutomaticWorkers(ctx, scopes, func(status string) { t.Log(status) }, func(_ context.Context, provider, _ string) bool { return provider == "codex" })
	}()
	defer func() { cancel(); <-done }()
	var target storage.RegisteredTargetIdentity
	wait(func() bool {
		targets, err := client.Targets(ctx, "demo")
		if err == nil && len(targets) == 1 {
			target = targets[0].Target
		}
		return target.Origin.SessionID != ""
	})
	task, err := client.CreateRoutedTask(ctx, "demo", gatewayclient.TaskCreateInput{Title: "Rust task stream fixture"}, &remote, gatewayclient.TaskRouting{Mode: gatewayclient.TaskRoutingUnassigned})
	if err != nil {
		t.Fatal(err)
	}
	wait(func() bool {
		_, cursor, err := local.WorkerTaskConsumer(ctx, client.Endpoint())
		return err == nil && cursor > 0
	})
	disconnect()
	<-manager.taskStreamDone
	if _, err := client.AssignTask(ctx, "demo", task.ID, target); err != nil {
		t.Fatal(err)
	}
	reconnected := make(chan struct{})
	go func() {
		defer close(reconnected)
		worker.WatchAssignments(ctx, local, client)
	}()
	defer func() { cancel(); <-reconnected }()
	wait(func() bool {
		threads, err := listLocalTaskThreads(root, "demo", task.ID)
		if err != nil || len(threads) != 1 {
			return false
		}
		data, err := os.ReadFile(filepath.Join(root, "demo", "tasks", task.ID, target.Origin.SessionID+".md"))
		return err == nil && strings.Contains(string(data), "> Local reply")
	})

	app := fynetest.NewApp()
	defer app.Quit()
	queue := newUICallbacks()
	browser, err := NewBrowser(BrowserConfig{Client: client, LocalConversationsDirectory: root, Dispatch: queue.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { browser.Close(); browser.Wait(); queue.pump() }()
	browser.Start(ctx)
	wait(func() bool {
		queue.pump()
		return browser.selectedProject == "demo" && len(browser.visibleTasks) == 1
	})
	browser.tasks.Select(0)
	wait(func() bool {
		queue.pump()
		return len(browser.conversationItems) == 1
	})
	browser.conversations.Select(0)
	conversationID := legacyTaskConversationID(task.ID, target.Origin.SessionID)
	chat := browser.chats[chatKey("demo", conversationID)]
	if chat == nil || browser.mainView != "Conversations" || browser.selection.ProjectConversation != conversationID {
		t.Fatal("local assignment conversation did not open in Task Thread")
	}
	wait(func() bool {
		queue.pump()
		return chat.connection.Text == "Local task transcript"
	})
	if got := opens.Load(); got != 1 {
		t.Fatalf("opened %d providers for one replayed assignment", got)
	}

	if cli := os.Getenv("AMUX_GO_TEST_BINARY"); cli != "" {
		owner := target.Origin
		mcp := exec.CommandContext(ctx, cli, "mcp", "--enrolled-profile", peer.ProfilePath, "--project", "demo",
			"--conversation", conversationID, "--local-task", task.ID, "--session", owner.SessionID,
			"--instance", owner.InstanceID, "--provider", owner.Provider, "--os", owner.OS)
		mcp.Stdin = strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}` + "\n" +
				`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"task_comment","arguments":{"id":"` + task.ID + `","body":"Rust task worker tool verified"}}}` + "\n")
		output, err := mcp.Output()
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Result *storage.ToolResult `json:"result"`
		}
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil || result.Result == nil || result.Result.IsError {
			t.Fatalf("Rust worker tool failed: %s", output)
		}
		detail, err := client.TaskDetail(ctx, "demo", task.ID)
		if err != nil || len(detail.Comments) == 0 || detail.Comments[len(detail.Comments)-1].Content != "Rust task worker tool verified" {
			t.Fatalf("Rust gateway did not store the provider tool comment: %v", err)
		}
	}
	if _, err := client.CloseTask(ctx, "demo", task.ID); err != nil {
		t.Fatal(err)
	}
	auto, err := client.CreateTask(ctx, "demo", gatewayclient.TaskCreateInput{Title: "Rust automatic assignment fixture"})
	if err != nil {
		t.Fatal(err)
	}
	wait(func() bool {
		threads, err := listLocalTaskThreads(root, "demo", auto.ID)
		return err == nil && len(threads) == 1 && threads[0].Owner == target.Origin && opens.Load() == 2
	})
}
