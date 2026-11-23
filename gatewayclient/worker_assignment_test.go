package gatewayclient

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestWorkerAssignmentHTTPRetryPreservesConversationAndPrompt(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Inspect this task", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	now := time.Now()
	activity := now.Add(-time.Minute).UnixMilli()
	if _, err := s.ReportPresence(ctx, storage.PresenceReport{Source: "pool/slot", Sequence: 1, Agents: []storage.PresenceAgent{{ProjectIdent: "demo", Origin: owner, AutoAssign: true, LastActivityAt: &activity}}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	c, err := New(host.URL, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.AdoptWorkerAssignment(ctx, "demo", owner)
	if err != nil || a == nil || a.Task.ID != task.ID {
		t.Fatalf("adoption %+v %v", a, err)
	}
	b, err := c.AdoptWorkerAssignment(ctx, "demo", owner)
	if err != nil || b == nil || b.RunID != a.RunID || b.Conversation.ID != a.Conversation.ID {
		t.Fatalf("retry %+v %v", b, err)
	}
	commands, err := s.ConversationCommands(ctx, "demo", a.Conversation.ID, 0, 100)
	if err != nil || len(commands) != 1 || commands[0].Command.Kind != "prompt" {
		t.Fatalf("commands %+v %v", commands, err)
	}
	foreign := owner
	foreign.InstanceID = uuid.NewString()
	if got, err := c.AdoptWorkerAssignment(ctx, "demo", foreign); err != nil || got != nil {
		t.Fatalf("foreign adoption %+v %v", got, err)
	}
}
