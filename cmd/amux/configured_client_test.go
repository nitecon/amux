package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/clientconfig"
	"github.com/nitecon/amux/internal/clientcontrol"
	"github.com/nitecon/amux/internal/localconversation"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
)

func TestConfiguredBindingsMatchFirstCheckoutAndRegisterOnlyExplicitProjects(t *testing.T) {
	root := t.TempDir()
	remote := "https://github.com/nitecon/sample"
	makeConfiguredCheckout(t, root, "sample.bak", remote)
	first := makeConfiguredCheckout(t, root, "sample", remote)
	makeConfiguredCheckout(t, root, "sample_gorebuild", remote)
	newRemote := "https://github.com/nitecon/new-repo"
	newCheckout := makeConfiguredCheckout(t, root, "new-repo", newRemote)
	var registered atomic.Bool
	second := makeConfiguredCheckout(t, t.TempDir(), "second", remote)
	wrong := makeConfiguredCheckout(t, t.TempDir(), "wrong", "https://github.com/nitecon/wrong")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["ident"] != "new-repo" || body["repo_url"] != "github.com/nitecon/new-repo" {
				http.Error(w, "unexpected registration", http.StatusBadRequest)
				return
			}
			registered.Store(true)
			return
		}
		inventory := []storage.ProjectOverview{{Project: storage.Project{Ident: "sample", CanonicalRemote: &remote}}}
		if registered.Load() {
			inventory = append(inventory, storage.ProjectOverview{Project: storage.Project{Ident: "new-repo", CanonicalRemote: &newRemote}})
		}
		_ = json.NewEncoder(w).Encode(inventory)
	}))
	defer server.Close()
	client, err := gatewayclient.New(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	config := clientconfig.Config{ProjectsDirectory: root}
	ctx := context.Background()
	bindings, err := configuredBindings(ctx, client, config)
	if err != nil || len(bindings) != 1 || bindings["sample"] != first {
		t.Fatalf("discovery did not select the first matching checkout only: %v, %v", bindings, err)
	}
	config.ProjectURLs = []string{remote + ".git"}
	bindings, err = configuredBindings(ctx, client, config)
	if err != nil || bindings["sample"] != first {
		t.Fatalf("URL did not resolve existing checkout: %v, %v", bindings, err)
	}
	config.Projects = map[string]string{"sample": second}
	bindings, err = configuredBindings(ctx, client, config)
	if err != nil || bindings["sample"] != second {
		t.Fatalf("moved %v, %v; expected %s", bindings, err, second)
	}
	config.Projects["sample"] = wrong
	if _, err := configuredBindings(ctx, client, config); err == nil {
		t.Fatal("accepted a checkout for a different repository")
	}
	config.Projects = nil
	config.ProjectURLs = []string{newRemote}
	bindings, err = configuredBindings(ctx, client, config)
	if err != nil || len(bindings) != 2 || bindings["new-repo"] != newCheckout {
		t.Fatalf("explicit registration did not bind its checkout: %v, %v", bindings, err)
	}
}

func makeConfiguredCheckout(t *testing.T, root, name, remote string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	for _, args := range [][]string{{"init", "--quiet", dir}, {"-C", dir, "remote", "add", "origin", remote}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	checkout, err := projectidentity.ResolveCheckout(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return checkout.Directory
}

func TestConfiguredClientMovesCheckoutAfterActiveTask(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("uses a POSIX test home")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AMUX_GO_ACP_TEST", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	const project = "checkout-reload"
	profile := t.TempDir()
	remote := "https://github.com/nitecon/checkout-reload"
	db, err := storage.Open(ctx, filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertProject(ctx, storage.Project{Ident: project, ChannelName: project, Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	address, stopGateway := startServerCommand(t, "--profile", profile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", uuid.NewString(), "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stopGateway()
	var invitation bytes.Buffer
	if err := run(ctx, []string{"server", "peering-key", "--profile", profile}, &invitation, io.Discard, os.Getenv); err != nil {
		t.Fatal(err)
	}
	enroll := func() string {
		t.Helper()
		peer, err := storage.PairRemote(ctx, t.TempDir(), "https://"+address, strings.TrimSpace(invitation.String()))
		if err != nil {
			t.Fatal(err)
		}
		return peer.ProfilePath
	}
	workerProfile, operatorProfile := enroll(), enroll()
	operator, err := gatewayclient.NewEnrolledFromManifest("https://"+address, operatorProfile)
	if err != nil {
		t.Fatal(err)
	}
	projectsRoot := t.TempDir()
	first := makeConfiguredCheckout(t, projectsRoot, "a-old", remote)
	second := makeConfiguredCheckout(t, projectsRoot, "z-new", remote)
	journal := filepath.Join(t.TempDir(), "workers.db")
	agent, err := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeACPHelper$"})
	if err != nil {
		t.Fatal(err)
	}
	var poolIDs []string
	for _, dir := range []string{first, second} {
		var output bytes.Buffer
		args := []string{"-add-project", dir, "--state", journal, "--enrolled-profile", workerProfile, "--project", project, "--agent-command", string(agent)}
		if err := run(ctx, args, &output, io.Discard, os.Getenv); err != nil {
			t.Fatal(err)
		}
		poolIDs = append(poolIDs, strings.TrimSpace(strings.TrimPrefix(output.String(), "worker pool ")))
	}
	owners := make([]storage.AgentOrigin, 0, len(poolIDs))
	workerStore, err := storage.Open(ctx, journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, poolID := range poolIDs {
		slots, err := workerStore.WorkerPoolSlots(ctx, poolID)
		if err != nil || len(slots) != 1 {
			t.Fatalf("saved worker %s: %+v, %v", poolID, slots, err)
		}
		owners = append(owners, slots[0].Owner)
	}
	workerStore.Close()
	configPath := filepath.Join(home, ".amux", "config.yml")
	config := clientconfig.Config{EnrolledProfile: workerProfile, WorkerJournal: journal, Providers: []string{"antigravity"}}
	if err := clientconfig.Save(configPath, config); err != nil {
		t.Fatal(err)
	}

	clientCtx, stopClient := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runHeadlessClient(clientCtx, nil, io.Discard, io.Discard) }()
	defer func() {
		stopClient()
		if err := <-done; err != nil {
			t.Errorf("configured client: %v", err)
		}
	}()
	waitFor := func(message string, ready func() bool) {
		t.Helper()
		for !ready() {
			select {
			case <-ctx.Done():
				t.Fatal(message)
			case <-time.After(25 * time.Millisecond):
			}
		}
	}
	findTarget := func(owner storage.AgentOrigin) *storage.RegisteredTargetIdentity {
		targets, err := operator.Targets(ctx, project)
		if err == nil {
			for _, registered := range targets {
				if registered.Target.Origin.SessionID == owner.SessionID {
					return &registered.Target
				}
			}
		}
		return nil
	}
	beginTask := func(title string, owner storage.AgentOrigin, dir string) (string, string) {
		t.Helper()
		waitFor("worker did not register for "+dir, func() bool { return findTarget(owner) != nil })
		task, err := operator.CreateTask(ctx, project, gatewayclient.TaskCreateInput{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		waitFor("task did not enter expected checkout "+dir, func() bool {
			_, err := os.Stat(filepath.Join(dir, "prompts"))
			return err == nil
		})
		var thread string
		waitFor("worker did not create its conversation", func() bool {
			conversations, err := operator.TaskConversations(ctx, project, task.ID, "", 10)
			if err == nil && len(conversations) == 1 {
				thread = conversations[0].ID
			}
			return thread != ""
		})
		return task.ID, thread
	}

	waitFor("client did not start without a project", func() bool {
		_, err := os.Stat(configPath + ".control")
		return err == nil
	})
	if findTarget(owners[0]) != nil || findTarget(owners[1]) != nil {
		t.Fatal("client registered a worker for an unconfigured project")
	}
	update, err := json.Marshal(clientcontrol.CheckoutChange{ProjectsDirectory: &projectsRoot})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientcontrol.Request(ctx, configPath, http.MethodPatch, "/checkout", update); err != nil {
		t.Fatalf("desktop settings update: %v", err)
	}
	config, err = clientconfig.Load(configPath)
	if err != nil || config.ProjectsDirectory != projectsRoot {
		t.Fatalf("desktop settings were not saved to YAML: %+v, %v", config, err)
	}
	applyBindings := func(bindings map[string]string) string {
		t.Helper()
		config.Projects = bindings
		if err := clientconfig.Save(configPath, config); err != nil {
			t.Fatal(err)
		}
		var result bytes.Buffer
		if err := requestClientReloadAt(ctx, configPath, &result); err != nil {
			t.Fatal(err)
		}
		return result.String()
	}
	if result := applyBindings(map[string]string{project: first}); !strings.Contains(result, "Applied 1 project bindings") {
		t.Fatalf("add checkout: %s", result)
	}
	attached, err := clientcontrol.PrepareCheckout(ctx, configPath, clientcontrol.CheckoutRequest{
		Project: project, Method: projectidentity.OpenInFolder, Destination: first,
	})
	if err != nil || attached.Identity.Directory != first {
		t.Fatalf("client checkout returned %+v, %v", attached, err)
	}
	firstTask, firstThread := beginTask("Task held at approval while checkout moves", owners[0], first)
	badConfig := []byte("unknown: invalid\n")
	if err := os.WriteFile(configPath, badConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if err := requestClientReloadAt(ctx, configPath, io.Discard); err == nil {
		t.Fatal("accepted an invalid configuration")
	}
	snapshot, err := clientcontrol.LoadWorkspace(ctx, configPath)
	if err != nil || snapshot.Projects[project] != first {
		t.Fatalf("rejected YAML changed the running workspace: %+v, %v", snapshot, err)
	}
	if result := applyBindings(map[string]string{project: second}); !strings.Contains(result, "draining ["+project+"/antigravity]") {
		t.Fatalf("move checkout: %s", result)
	}
	if target := findTarget(owners[1]); target != nil {
		t.Fatal("replacement registered before the active task finished")
	}
	finishTask := func(task, thread, directory string, owner storage.AgentOrigin) {
		t.Helper()
		streamCtx, stopStream := context.WithTimeout(ctx, 5*time.Second)
		defer stopStream()
		seenPermission := errors.New("saw permission")
		remote := localconversation.Remote{ConfigPath: configPath}
		var permission string
		err := remote.WatchApprovals(streamCtx, func(approvals []localconversation.Approval) error {
			for _, approval := range approvals {
				if approval.Task && approval.Conversation.ID == thread {
					permission = approval.Event.ID
					return seenPermission
				}
			}
			return nil
		})
		if !errors.Is(err, seenPermission) || permission == "" {
			t.Fatalf("draining checkout did not retain its active approval: %v", err)
		}
		if err := remote.ResolveTaskApproval(ctx, permission, "reject"); err != nil {
			t.Fatal(err)
		}
		transcript, err := localconversation.TaskDirectory(filepath.Join(home, ".amux", "conversations"), project, task)
		if err != nil {
			t.Fatal(err)
		}
		waitFor("task did not finish in "+directory, func() bool {
			header, events, err := localconversation.Read(filepath.Join(transcript, owner.SessionID+".md"), true)
			if err != nil || header.Directory != directory {
				return false
			}
			for _, event := range events {
				if event.Event.Kind == "turn_completed" {
					return true
				}
			}
			return false
		})
		if _, err := operator.TaskUpdate(ctx, project, task, storage.TaskWorkflowUpdate{Status: "done"}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	finishTask(firstTask, firstThread, first, owners[0])
	secondTask, secondThread := beginTask("Next task runs in the new checkout", owners[1], second)
	config.ProjectsDirectory = ""
	if result := applyBindings(nil); !strings.Contains(result, "Applied 0 project bindings; draining ["+project+"/antigravity]") {
		t.Fatalf("remove checkout: %s", result)
	}
	finishTask(secondTask, secondThread, second, owners[1])
	snapshot, err = clientcontrol.LoadWorkspace(ctx, configPath)
	if err != nil || len(snapshot.Projects) != 0 || len(snapshot.LocalCheckouts) != 0 {
		t.Fatalf("removed binding was not kept separate from discovered checkouts: %+v, %v", snapshot, err)
	}
	waitFor("removed worker still advertised itself for assignments", func() bool {
		workers, err := operator.Presence(ctx, project)
		if err != nil {
			return false
		}
		for _, worker := range workers {
			if worker.State != "gone" {
				return false
			}
		}
		return true
	})
	applyBindings(map[string]string{project: first})
	var chat *storage.FleetConversation
	waitFor("operator could not open an enrolled fleet chat", func() bool {
		opened, err := operator.OpenFleetConversation(ctx, project, owners[0].SessionID)
		if err == nil {
			chat = opened
		}
		return chat != nil
	})
	const prompt = "Read the fixture checkout"
	if err := operator.Enqueue(ctx, project, chat.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: prompt}); err != nil {
		t.Fatal(err)
	}
	streamCtx, stopStream := context.WithTimeout(ctx, 8*time.Second)
	defer stopStream()
	finished := errors.New("fleet turn completed")
	receivedInput, receivedOutput := false, false
	err = operator.Stream(streamCtx, project, chat.ID, gatewayclient.Cursor{}, func(frame gatewayclient.Frame) error {
		if frame.Event == nil {
			return nil
		}
		switch frame.Event.Event.Kind {
		case "user_message":
			receivedInput = frame.Event.Event.Text == prompt
		case "text_delta":
			receivedOutput = frame.Event.Event.Text == "Inspecting headless task"
		case "permission":
			return operator.Enqueue(streamCtx, project, chat.ID, uuid.NewString(), conversation.Command{
				Kind: "permission", PermissionEventID: frame.Event.ID, OptionID: "reject",
			})
		case "turn_completed":
			return finished
		}
		return nil
	})
	if !errors.Is(err, finished) || !receivedInput || !receivedOutput {
		t.Fatalf("fleet turn got input %v, output %v, err %v", receivedInput, receivedOutput, err)
	}
	fleetFile := filepath.Join(home, ".amux", "conversations", project, "fleet", chat.ID+".md")
	header, _, err := localconversation.Read(fleetFile, true)
	if err != nil || header.Directory != first {
		t.Fatalf("fleet conversation ran in %q instead of %q: %v", header.Directory, first, err)
	}
	reopened, err := operator.OpenFleetConversation(ctx, project, owners[0].SessionID)
	if err != nil || reopened.ID != chat.ID {
		t.Fatalf("default fleet conversation changed: %+v, %v", reopened, err)
	}

}
