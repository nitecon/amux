package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestTasksUpdateDelegationUsesPinnedSDKReceiptAndLiteralFlags(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "delegation-update.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, ident := range []string{"source", "target"} {
		if err := store.InsertProject(ctx, storage.Project{Ident: ident, ChannelName: "none", Kind: "adhoc"}); err != nil {
			t.Fatal(err)
		}
	}
	actor := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	rootReason, details := "user-directed", "Edit linked contract"
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "source", Title: "Parent", Reporter: actor.SessionID, RootReason: &rootReason, Details: &details}, &actor)
	if err != nil {
		t.Fatal(err)
	}
	delegated, err := store.DelegateTask(ctx, storage.TaskDelegationInput{
		SourceProject: "source", TargetProject: "target", Title: "Initial", Reporter: actor.SessionID, ParentID: &parent.ID, Labels: []string{"priority:low"},
	}, &actor)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "delegation-update-key")
	if err != nil {
		t.Fatal(err)
	}
	var patches atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/v1/projects/source/tasks/"+delegated.SourceTask.ID+"/delegation" {
			patches.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

	args := []string{
		"--server", host.URL, "--project", "source",
		"--session", actor.SessionID, "--instance", actor.InstanceID, "--provider", actor.Provider, "--os", actor.OS,
		"update-delegation", delegated.SourceTask.ID,
		"--title", "Updated literal", "--description", "", "--specification", "", "--details", "ignored alias", "--labels", "[]",
	}
	var stdout, stderr bytes.Buffer
	err = runTasks(ctx, args, &stdout, &stderr, func(name string) string {
		if name == "GATEWAY_API_KEY" {
			return "delegation-update-key"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("runTasks: %v stderr=%s", err, stderr.String())
	}
	if patches.Load() != 1 {
		t.Fatalf("uncertain mutation retry count=%d", patches.Load())
	}
	var receipt storage.TaskDelegationResult
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil {
		t.Fatalf("CLI output is not one JSON receipt: %q: %v", stdout.String(), err)
	}
	if receipt.SourceTask.ID != delegated.SourceTask.ID || receipt.TargetTask.ID != delegated.TargetTask.ID || receipt.MessageID <= 0 || receipt.SourceTask.Title != "Updated literal (DELEGATED)" || receipt.TargetTask.Title != "Updated literal" || receipt.SourceTask.Description == nil || *receipt.SourceTask.Description != "" || receipt.TargetTask.Description == nil || *receipt.TargetTask.Description != "" || receipt.SourceTask.Details == nil || *receipt.SourceTask.Details != "" || receipt.TargetTask.Details == nil || *receipt.TargetTask.Details != "" || receipt.SourceTask.Labels == nil || receipt.TargetTask.Labels == nil || len(receipt.SourceTask.Labels) != 0 || len(receipt.TargetTask.Labels) != 0 {
		t.Fatalf("CLI receipt did not confirm the exact mirrored update: %+v", receipt)
	}
}

func TestTasksUpdateDelegationRequiresNativeOriginBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer host.Close()
	args := []string{"--server", host.URL, "--project", "source", "update-delegation", "11111111-1111-4111-8111-111111111111", "--title", "Update"}
	var stdout, stderr bytes.Buffer
	err := runTasks(context.Background(), args, &stdout, &stderr, func(string) string { return "key" })
	if err == nil || requests.Load() != 0 {
		t.Fatalf("missing native origin err=%v requests=%d", err, requests.Load())
	}
}
