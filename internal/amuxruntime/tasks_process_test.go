package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestTaskCLIProcessHelper(t *testing.T) {
	if os.Getenv("AMUX_TASK_CLI_PROCESS_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			if err := run(context.Background(), os.Args[i+1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
	os.Exit(3)
}

func TestHeadlessTaskCLIProcessPreservesNativeWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	submissionFixtureGit(t, checkout, "init", "--initial-branch=main", "--template=")
	const remoteValue = "github.com/example/task-cli-fixture"
	submissionFixtureGit(t, checkout, "remote", "add", "origin", "https://"+remoteValue+".git")
	identity, err := projectidentity.Resolve(ctx, checkout)
	if err != nil || identity.Upstream == "" {
		t.Fatalf("resolve test checkout binding: identity=%+v err=%v", identity, err)
	}
	remote := identity.Upstream
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "cli-fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	var mutations atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer host.Close()
	common := []string{"tasks", "--server", host.URL, "--project", "demo"}
	native := append(append([]string{}, common...), "--session", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "--instance", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "--provider", "codex", "--os", "linux", "--goal", "release")
	invoke := func(prefix []string, args ...string) ([]byte, string, error) {
		t.Helper()
		return invokeTaskCLIProcessAt(ctx, checkout, prefix, args...)
	}
	if _, _, err := invoke(common, "create", "Must not implicitly become human", "--root-reason", "user-directed", "--specification", "Authorization test"); err == nil || mutations.Load() != 0 {
		t.Fatalf("missing origin wrote task: mutations=%d err=%v", mutations.Load(), err)
	}
	if raw, diagnostics, err := invoke(common, "list", "--help"); err != nil || len(raw) != 0 || !strings.Contains(diagnostics, "status") {
		t.Fatalf("operation help missing: %q %q %v", raw, diagnostics, err)
	}
	raw, diagnostics, err := invoke(native, "create", "CLI fixture", "--root-reason", "user-directed", "--specification", "Verify console task workflow", "--os", "linux")
	if err != nil {
		t.Fatalf("create: %v %s", err, diagnostics)
	}
	var task storage.Task
	if err := json.Unmarshal(raw, &task); err != nil || task.Goal == nil || *task.Goal != "release" || task.OwnerOrigin == nil {
		t.Fatalf("create result: %s %v", raw, err)
	}
	taskID := task.ID
	for _, operation := range [][]string{{"claim", taskID}, {"started", taskID}, {"rank", taskID, "-7"}, {"set-os", taskID, "--os", "windows"}} {
		if _, stderr, err := invoke(native, operation...); err != nil {
			t.Fatalf("%v: %v %s", operation, err, stderr)
		}
	}
	detail, err := store.Task(ctx, "demo", taskID)
	if err != nil || detail.Task.Status != "todo" || detail.Task.OwnerOrigin != nil || detail.Task.Rank != -7 {
		t.Fatalf("set-os did not release incompatible owner: %+v %v", detail, err)
	}
	reported := false
	for _, comment := range detail.Comments {
		if strings.Contains(comment.Content, "execution started") && comment.Origin != nil && comment.Author == comment.Origin.SessionID {
			reported = true
		}
	}
	if !reported {
		t.Fatal("started did not record native execution report")
	}
	if _, stderr, err := invoke(native, "set-os", taskID, "--clear"); err == nil || !strings.Contains(stderr, "exact owning agent") {
		t.Fatalf("former owner retag unexpectedly succeeded: %v %s", err, stderr)
	}
	windowsNative := append([]string(nil), native...)
	for i := 0; i < len(windowsNative)-1; i++ {
		switch windowsNative[i] {
		case "--session":
			windowsNative[i+1] = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
		case "--instance":
			windowsNative[i+1] = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
		case "--os":
			windowsNative[i+1] = "windows"
		}
	}
	for _, operation := range [][]string{{"claim", taskID}, {"set-os", taskID, "--clear"}} {
		if _, stderr, err := invoke(windowsNative, operation...); err != nil {
			t.Fatalf("Windows execution %v: %v %s", operation, err, stderr)
		}
	}
	if _, stderr, err := invoke(windowsNative, "done", taskID); err == nil || !strings.Contains(stderr, "set-changeset") {
		t.Fatalf("missing guided confirmation: %v %s", err, stderr)
	}
	if _, stderr, err := invoke(windowsNative, "confirm-mutation", taskID); err != nil {
		t.Fatalf("confirm: %v %s", err, stderr)
	}
	if _, stderr, err := invoke(windowsNative, "set-changeset", taskID, `["cmd/main.go"]`); err != nil {
		t.Fatalf("changeset: %v %s", err, stderr)
	}
	raw, diagnostics, err = invoke(common, "get", taskID, "--comments", "1")
	if err != nil {
		t.Fatalf("get: %v %s", err, diagnostics)
	}
	if err := json.Unmarshal(raw, &task); err != nil || task.ID != taskID || task.Status != "done" || len(task.Changeset) != 1 {
		t.Fatalf("completed task: %s %v", raw, err)
	}
}

func invokeTaskCLIProcess(ctx context.Context, prefix []string, args ...string) ([]byte, string, error) {
	return invokeTaskCLIProcessAt(ctx, "", prefix, args...)
}

func invokeTaskCLIProcessAt(ctx context.Context, directory string, prefix []string, args ...string) ([]byte, string, error) {
	return invokeTaskCLIProcessWithEnvAt(ctx, directory, map[string]string{"GATEWAY_API_KEY": "cli-fixture-key"}, prefix, args...)
}

func invokeTaskCLIProcessWithEnvAt(ctx context.Context, directory string, extraEnv map[string]string, prefix []string, args ...string) ([]byte, string, error) {
	commandArgs := append([]string{"-test.run=^TestTaskCLIProcessHelper$", "--"}, prefix...)
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, os.Args[0], commandArgs...)
	if directory != "" {
		command.Dir = directory
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := extraEnv[name]; overridden {
			continue
		}
		if name != "GATEWAY_API_KEY" && name != "AMUX_TASK_CLI_PROCESS_TEST" && name != "DISPLAY" && name != "WAYLAND_DISPLAY" && name != "DBUS_SESSION_BUS_ADDRESS" {
			command.Env = append(command.Env, entry)
		}
	}
	for name, value := range extraEnv {
		command.Env = append(command.Env, name+"="+value)
	}
	command.Env = append(command.Env, "AMUX_TASK_CLI_PROCESS_TEST=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.String(), err
}

func TestHeadlessTaskCancellationProcessPreservesReportAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cancel-cli.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actor := storage.AgentOrigin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "antigravity", OS: "linux"}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Cancel via console", Reporter: "user"}, &actor)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "cli-fixture-key")
	if err != nil {
		t.Fatal(err)
	}
	var mutations atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			mutations.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer host.Close()
	common := []string{"tasks", "--server", host.URL, "--project", "demo"}
	native := append(append([]string{}, common...), "--session", actor.SessionID, "--instance", actor.InstanceID, "--provider", actor.Provider, "--os", actor.OS)
	human := append(append([]string{}, common...), "--human")
	if _, _, err := invokeTaskCLIProcess(ctx, human, "cancel", task.ID, "--reason", "Stop now", "--changeset", "null"); err == nil || mutations.Load() != 0 {
		t.Fatalf("human cancellation wrote: %d %v", mutations.Load(), err)
	}
	if _, diagnostics, err := invokeTaskCLIProcess(ctx, native, "cancel", task.ID, "--reason", "Stop now"); err == nil || !strings.Contains(diagnostics, "--changeset") {
		t.Fatalf("missing cancellation confirmation: %v %s", err, diagnostics)
	}
	raw, diagnostics, err := invokeTaskCLIProcess(ctx, native, "cancel", task.ID, "--reason", "Stop now", "--changeset", "null")
	if err != nil {
		t.Fatalf("cancel: %v %s", err, diagnostics)
	}
	var result storage.CancelResponse
	if err := json.Unmarshal(raw, &result); err != nil || result.Task == nil || result.Cancellation == nil || result.Task.Status != "cancelled" || result.Cancellation.ExecutorStopConfirmed || result.Cancellation.ReporterOrigin != actor || result.Cancellation.PriorOwnerOrigin == nil || *result.Cancellation.PriorOwnerOrigin != actor {
		t.Fatalf("cancellation acknowledgement: %s %v", raw, err)
	}
	if _, diagnostics, err := invokeTaskCLIProcess(ctx, native, "cancel", task.ID, "--reason", "Stop now"); err != nil {
		t.Fatalf("same cancellation replay without changeset: %v %s", err, diagnostics)
	}
	events, err := store.TaskEvents(ctx, storage.EventQuery{TaskID: &task.ID})
	if err != nil || len(events) != 2 {
		t.Fatalf("replay changed lifecycle history: %+v %v", events, err)
	}
	raw, diagnostics, err = invokeTaskCLIProcess(ctx, common, "cancellation", task.ID)
	var report storage.Cancellation
	if err != nil {
		t.Fatalf("read cancellation: %v %s", err, diagnostics)
	}
	if err := json.Unmarshal(raw, &report); err != nil || report.TaskID != task.ID || report.Reason != "Stop now" || report.ExecutorStopConfirmed {
		t.Fatalf("read cancellation changed report: %s %v", raw, err)
	}
}
