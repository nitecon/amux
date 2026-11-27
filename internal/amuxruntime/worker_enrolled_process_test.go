package amuxruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func TestHeadlessEnrolledWorkerProcessStartsAndResumesPairedProfile(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX signal smoke test; Windows service lifecycle is separate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	serverProfile := t.TempDir()
	serverStore, err := storage.Open(ctx, filepath.Join(serverProfile, "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := serverStore.InsertProject(ctx, storage.Project{Ident: "paired-worker", ChannelName: "paired-worker", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := serverStore.InsertTask(ctx, storage.TaskInsert{Project: "paired-worker", Title: "Paired worker", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := serverStore.Close(); err != nil {
		t.Fatal(err)
	}
	address, stopServer := startServerCommand(t, "--profile", serverProfile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", uuid.NewString(), "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stopServer()
	var invitation bytes.Buffer
	if err := run(context.Background(), []string{"server", "peering-key", "--profile", serverProfile}, &invitation, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	paired, err := storage.PairRemote(ctx, t.TempDir(), "https://"+address, strings.TrimSpace(invitation.String()))
	if err != nil {
		t.Fatalf("pair remote worker profile: %v", err)
	}
	if filepath.Base(paired.ProfilePath) != "client-profile.json" {
		t.Fatalf("pairing emitted unexpected profile path: %s", paired.ProfilePath)
	}
	client, err := gatewayclient.NewEnrolledFromManifest(paired.Profile.URL, paired.ProfilePath)
	if err != nil {
		t.Fatalf("load paired profile through gateway client: %v", err)
	}

	state, checkout := filepath.Join(t.TempDir(), "worker.db"), t.TempDir()
	agent, _ := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeACPHelper$"})
	start := func(args ...string) *workerProcess {
		env := map[string]string{
			"AMUX_GO_ACP_TEST":              "1",
			"AMUX_GO_EXPECT_ENROLLED_TOOLS": "1",
			"AMUX_GO_EXPECT_CLIENT_PROFILE": paired.ProfilePath,
			"GATEWAY_API_KEY":               "must-not-reach-the-enrolled-worker",
		}
		processArgs := []string{"--enrolled-profile", paired.ProfilePath}
		if len(args) == 0 {
			processArgs = append(processArgs, "--project", "paired-worker", "--task", task.ID,
				"--directory", checkout, "--agent-command", string(agent))
		}
		processArgs = append(processArgs, args...)
		return startWorkerProcessModeWithTaskTools(t, ctx, state, env, "run", true, processArgs...)
	}
	waitForReady := func(conversationID string, expected int) {
		t.Helper()
		for {
			conversation, err := client.Conversation(ctx, "paired-worker", conversationID)
			if err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(filepath.Join(checkout, "enrolled-tool-calls"))
			count := strings.Count(string(calls), "task_list\n")
			if conversation.NativeSessionID == "native-headless" && count >= expected {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("worker reached native session %q and %d enrolled task calls, want session native-headless and %d calls", conversation.NativeSessionID, count, expected)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}

	first := start()
	waitForReady(first.conversationID, 1)
	stopWorkerProcess(t, ctx, first)
	resumed := start("--run-id", first.runID)
	if resumed.runID != first.runID || resumed.conversationID != first.conversationID {
		t.Fatal("enrolled worker resume replaced the saved task conversation")
	}
	waitForReady(first.conversationID, 2)
	stopWorkerProcess(t, ctx, resumed)
	if data, err := os.ReadFile(filepath.Join(checkout, "resumed")); err != nil || string(data) != "native-headless" {
		t.Fatalf("synthetic provider did not resume its exact native session: %q %v", data, err)
	}
}

// A separately enrolled operator uses the same gateway methods as the desktop
// assignment panel. The worker has only its own checkout, journal and identity.
func TestHeadlessClientAcceptsAssignmentFromAnotherEnrolledPeer(t *testing.T) {
	t.Setenv("AMUX_GO_ACP_TEST", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	profile := t.TempDir()
	const project = "remote-assignment"
	const missingProject = "missing-checkout"
	canonical := "github.com/rehearsal/remote-assignment"
	store, err := storage.Open(ctx, filepath.Join(profile, "application.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: project, Kind: "repo", CanonicalRemote: &canonical}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.InsertProject(ctx, storage.Project{Ident: missingProject, ChannelName: missingProject, Kind: "adhoc"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	address, stop := startServerCommand(t, "--profile", profile, "--profile-tls-name", "127.0.0.1",
		"--secure-instance-id", uuid.NewString(), "--listen", "127.0.0.1:0", "--discovery-bind", "127.0.0.1:0")
	defer stop()
	var key bytes.Buffer
	if err := run(ctx, []string{"server", "peering-key", "--profile", profile}, &key, io.Discard, os.Getenv); err != nil {
		t.Fatal(err)
	}
	enroll := func() string {
		t.Helper()
		peer, err := storage.PairRemote(ctx, t.TempDir(), "https://"+address, strings.TrimSpace(key.String()))
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
	checkout := t.TempDir()
	if output, err := exec.CommandContext(ctx, "git", "-C", checkout, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize checkout: %v: %s", err, output)
	}
	if output, err := exec.CommandContext(ctx, "git", "-C", checkout, "remote", "add", "origin", "https://"+canonical+".git").CombinedOutput(); err != nil {
		t.Fatalf("set checkout origin: %v: %s", err, output)
	}
	state := filepath.Join(t.TempDir(), "worker.sqlite")
	agent, err := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeACPHelper$"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"-add-project", checkout, "--state", state, "--enrolled-profile", workerProfile,
		"--project", project, "--agent-command", string(agent)}, io.Discard, io.Discard, os.Getenv); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "checkout")
	if output, err := exec.CommandContext(ctx, "git", "init", "-q", missing).CombinedOutput(); err != nil {
		t.Fatalf("initialize second checkout: %v: %s", err, output)
	}
	if err := run(ctx, []string{"-add-project", missing, "--state", state, "--enrolled-profile", workerProfile,
		"--project", missingProject}, io.Discard, io.Discard, os.Getenv); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(missing, missing+"-offline"); err != nil {
		t.Fatal(err)
	}
	task, err := operator.CreateTask(ctx, project, gatewayclient.TaskCreateInput{Title: "Run on the headless worker"})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- runHeadlessClient(workerCtx, []string{"--state", state, "--enrolled-profile", workerProfile}, io.Discard, io.Discard)
	}()
	defer func() {
		stopWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("headless client: %v", err)
		}
	}()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	var target storage.RegisteredTargetIdentity
	for target.Generation == "" {
		targets, err := operator.Targets(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) == 1 {
			target = targets[0].Target
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("operator did not see the headless target: %+v", targets)
		case <-tick.C:
		}
	}
	if _, err := operator.AssignTask(ctx, project, task.ID, target); err != nil {
		t.Fatal(err)
	}
	for {
		conversations, err := operator.TaskConversations(ctx, project, task.ID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(conversations) == 1 && conversations[0].NativeSessionID == "native-headless" {
			checkouts, err := operator.Checkouts(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range checkouts {
				if item.Ident == missingProject && len(item.UnavailableHosts) == 1 && len(item.Machines) == 0 {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("headless assignment or unavailable checkout report was not observed for task %s: %+v", task.ID, conversations)
		case <-tick.C:
		}
	}
}
