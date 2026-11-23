package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestTasksReassignCLITransfersTaskToRegisteredRecipient(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	checkout := filepath.Join(base, "checkout")
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	submissionFixtureGit(t, checkout, "init", "--initial-branch=main", "--template=")
	submissionFixtureGit(t, checkout, "config", "user.name", "Reassignment Fixture")
	submissionFixtureGit(t, checkout, "config", "user.email", "fixture@example.invalid")
	const canonicalRemote = "github.com/example/reassignment-fixture"
	if err := os.WriteFile(filepath.Join(checkout, "README"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	submissionFixtureGit(t, checkout, "add", "README")
	submissionFixtureGit(t, checkout, "commit", "-m", "fixture")
	submissionFixtureGit(t, checkout, "remote", "add", "origin", "https://"+canonicalRemote+".git")

	store, err := storage.Open(ctx, filepath.Join(base, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	remote := canonicalRemote
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "repo", CanonicalRemote: &remote}); err != nil {
		t.Fatal(err)
	}
	actor := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	recipient := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	task, err := store.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Transfer this task", Reporter: "fixture", Labels: []string{"keep:fixture", "os:linux"}}, &actor)
	if err != nil {
		t.Fatal(err)
	}
	generation := uuid.NewString()
	api, err := server.NewHandler(store, "reassign-e2e-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(api)
	t.Cleanup(host.Close)
	client, err := gatewayclient.New(host.URL, "reassign-e2e-key", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	registered, err := client.RegisterTarget(ctx, "demo", storage.TargetRegistration{
		Origin: recipient, Generation: generation, CanonicalRemote: &remote,
	})
	if err != nil {
		t.Fatal(err)
	}
	if registered.Target.Origin != recipient || registered.Target.Generation != generation {
		t.Fatalf("registered target identity=%+v", registered.Target)
	}

	// A session with no matching live target must be rejected before task
	// ownership or routing changes.
	args := []string{"--server", host.URL, "--project", "demo", "--session", actor.SessionID,
		"--instance", actor.InstanceID, "--provider", actor.Provider, "--os", actor.OS,
		"reassign", task.ID, "--to-agent", uuid.NewString()}
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(checkout); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })
	var stdout, stderr bytes.Buffer
	if err := runTasks(ctx, args, &stdout, &stderr, func(string) string { return "reassign-e2e-key" }); err == nil {
		t.Fatal("reassignment to an unregistered recipient unexpectedly succeeded")
	}
	unchanged, err := store.Task(ctx, "demo", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Task.Status != task.Status || unchanged.Task.OwnerOrigin == nil || *unchanged.Task.OwnerOrigin != actor || !bytes.Equal(unchanged.Task.Routing, task.Routing) {
		t.Fatalf("failed reassignment mutated task: %+v", unchanged.Task)
	}

	args[len(args)-1] = recipient.SessionID
	stdout.Reset()
	stderr.Reset()
	if err := runTasks(ctx, args, &stdout, &stderr, func(string) string { return "reassign-e2e-key" }); err != nil {
		t.Fatalf("reassignment CLI: %v\n%s", err, stderr.String())
	}
	var receipt storage.Task
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatalf("decode reassignment receipt %q: %v", stdout.String(), err)
	}
	if receipt.ID != task.ID || receipt.ProjectIdent != "demo" || receipt.Status != "todo" || receipt.OwnerOrigin == nil || *receipt.OwnerOrigin != recipient {
		t.Fatalf("reassignment receipt did not confirm recipient ownership: %+v", receipt)
	}
	var route struct {
		Mode   string                           `json:"mode"`
		Target storage.RegisteredTargetIdentity `json:"target"`
	}
	if err := json.Unmarshal(receipt.Routing, &route); err != nil || route.Mode != "session" || route.Target != registered.Target {
		t.Fatalf("receipt routing=%s err=%v", receipt.Routing, err)
	}

	// Use the gateway client for the recipient's authenticated HTTP claim.
	claimedJSON, err := client.TaskUpdate(ctx, "demo", task.ID, storage.TaskWorkflowUpdate{Status: "in_progress"}, &recipient, nil)
	if err != nil {
		t.Fatalf("recipient claim: %v", err)
	}
	var claimed storage.Task
	if err := json.Unmarshal(claimedJSON, &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.ID != task.ID || claimed.Status != "in_progress" || claimed.OwnerOrigin == nil || *claimed.OwnerOrigin != recipient {
		t.Fatalf("recipient claim=%+v", claimed)
	}
	stored, err := store.Task(ctx, "demo", task.ID)
	if err != nil || stored.Task.Status != "in_progress" || stored.Task.OwnerOrigin == nil || *stored.Task.OwnerOrigin != recipient {
		t.Fatalf("stored recipient claim=%+v err=%v", stored, err)
	}
	if claimed.StartedAt == nil {
		t.Fatal("recipient claim did not set its start timestamp")
	}

	setOSArgs := []string{"--server", host.URL, "--project", "demo", "--session", recipient.SessionID,
		"--instance", recipient.InstanceID, "--provider", recipient.Provider, "--os", recipient.OS,
		"set-os", task.ID, "--os", "windows"}
	stdout.Reset()
	stderr.Reset()
	if err := runTasks(ctx, setOSArgs, &stdout, &stderr, func(string) string { return "reassign-e2e-key" }); err != nil {
		t.Fatalf("recipient set-os CLI: %v\n%s", err, stderr.String())
	}
	var retagged storage.Task
	if err := json.Unmarshal(stdout.Bytes(), &retagged); err != nil {
		t.Fatalf("decode set-os receipt %q: %v", stdout.String(), err)
	}
	if retagged.ID != task.ID || retagged.Status != "todo" || retagged.OwnerAgentID != nil || retagged.OwnerOrigin != nil || retagged.StartedAt != nil || retagged.DoneAt != nil {
		t.Fatalf("incompatible owner or timestamps remained after OS retag: %+v", retagged)
	}
	if string(retagged.Routing) != `{"mode":"auto"}` {
		t.Fatalf("session routing was not reset: %s", retagged.Routing)
	}
	if len(retagged.Labels) != 2 || retagged.Labels[0] != "keep:fixture" || retagged.Labels[1] != "os:windows" {
		t.Fatalf("OS retag changed unrelated labels: %#v", retagged.Labels)
	}
	final, err := store.Task(ctx, "demo", task.ID)
	if err != nil || final.Task.OwnerOrigin != nil || final.Task.OwnerAgentID != nil || final.Task.StartedAt != nil || final.Task.Status != "todo" {
		t.Fatalf("stored OS retag state=%+v err=%v", final, err)
	}
}
