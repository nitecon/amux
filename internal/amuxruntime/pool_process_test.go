package amuxruntime

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestHeadlessPoolProcessAdoptsAndResumesWithoutRepeatingPrompt(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX process signal smoke test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "pool", ChannelName: "pool", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "pool", Title: "Inspect queued work", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(remote, "pool-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	defer h.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	client, err := gatewayclient.New(host.URL, "pool-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	state := filepath.Join(t.TempDir(), "pool.db")
	command, _ := json.Marshal([]string{os.Args[0], "-test.run=^TestNativeACPHelper$"})
	start := func(args ...string) *workerProcess {
		return startWorkerProcessMode(t, ctx, state, map[string]string{"AMUX_GO_ACP_TEST": "1", "GATEWAY_API_KEY": "pool-secret"}, "pool", args...)
	}
	p := start("--server", host.URL, "--project", "pool", "--directory", checkout, "--agent-command", string(command))
	poolID := p.poolID
	// Advance the injected server reconciliation clock to the initial idle
	// boundary instead of making this process test sleep for thirty seconds.
	for {
		presence, err := remote.Presence(ctx, "pool")
		if err != nil {
			t.Fatal(err)
		}
		if len(presence) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("pool did not advertise")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := remote.ReconcileAssignments(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	var conversationID string
	var permission storage.ConversationEvent
	for permission.ID == "" {
		changed := remote.ConversationChanges()
		conversations, err := remote.TaskConversations(ctx, "pool", task.ID, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(conversations) > 1 {
			t.Fatal("assignment created multiple conversations")
		}
		if len(conversations) == 1 {
			conversationID = conversations[0].ID
			events, err := remote.ConversationEvents(ctx, "pool", conversationID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Event.Kind == "permission" {
					permission = e
				}
			}
		}
		if permission.ID == "" {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatalf("assigned pool did not prompt: %s", p.stderr)
			}
		}
	}
	if err := client.Enqueue(ctx, "pool", conversationID, uuid.NewString(), conversation.Command{Kind: "permission", PermissionEventID: permission.ID, OptionID: "reject"}); err != nil {
		t.Fatal(err)
	}
	for {
		changed := remote.ConversationChanges()
		commands, err := remote.ConversationCommands(ctx, "pool", conversationID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		complete := len(commands) == 2
		for _, c := range commands {
			complete = complete && c.State == "done"
		}
		if complete {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("pool did not finish prompt")
		}
	}
	stopWorkerProcess(t, ctx, p)
	p = start("--pool-id", poolID)
	for {
		if _, err := os.Stat(filepath.Join(checkout, "resumed")); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("saved pool did not resume native session: %s", p.stderr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	stopWorkerProcess(t, ctx, p)
	prompts, err := os.ReadFile(filepath.Join(checkout, "prompts"))
	if err != nil || strings.Count(string(prompts), "prompt\n") != 1 {
		t.Fatalf("native prompt replayed: %q %v", prompts, err)
	}
	conversations, err := remote.TaskConversations(ctx, "pool", task.ID, "", 100)
	if err != nil || len(conversations) != 1 {
		t.Fatalf("restart duplicated conversation: %+v %v", conversations, err)
	}
}
