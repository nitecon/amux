package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestPrepareRunSupportsTasklessProjectConversation(t *testing.T) {
	ctx := context.Background()
	conversationID := uuid.NewString()
	var gotPath string
	var gotOwner agent.Origin
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotOwner = agent.Origin{
			SessionID: r.Header.Get("X-Agent-Session-Id"), InstanceID: r.Header.Get("X-Agent-Instance-Id"),
			Provider: r.Header.Get("X-Agent-Provider"), OS: r.Header.Get("X-Agent-OS"),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(storage.Conversation{ID: conversationID, ProjectIdent: "desktop", Owner: gotOwner})
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "secret", host.Client())
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkout := t.TempDir()
	canonicalCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	command := []string{"agent", "--stdio"}
	run, conversation, err := PrepareRun(ctx, store, client, "desktop", "", checkout, "codex", command, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/projects/desktop/conversations" {
		t.Fatalf("taskless conversation path = %q", gotPath)
	}
	if conversation.ID != conversationID || conversation.ProjectIdent != "desktop" || conversation.TaskID != "" {
		t.Fatalf("conversation = %+v", conversation)
	}
	if run.ID == "" || run.ConversationID != conversationID || run.Project != "desktop" || run.Server != client.Endpoint() || run.Directory != canonicalCheckout || run.Owner != gotOwner || run.Owner.Provider != "codex" || len(gotOwner.SessionID) == 0 || len(gotOwner.InstanceID) == 0 {
		t.Fatalf("worker run = %+v, owner = %+v", run, gotOwner)
	}
	if len(run.Command) != len(command) || run.Command[0] != command[0] || run.Command[1] != command[1] {
		t.Fatalf("worker command = %#v", run.Command)
	}
	saved, err := store.WorkerRun(ctx, run.ID)
	if err != nil || saved == nil || saved.ConversationID != conversationID || saved.Owner != run.Owner {
		t.Fatalf("saved worker run = %+v, err = %v", saved, err)
	}
}
