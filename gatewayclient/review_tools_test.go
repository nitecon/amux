package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestReviewTaskToolsPreferAvailablePeerAndPermitFallback(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "reviews.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "reviews", ChannelName: "reviews", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	owner := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	peer := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	source, err := s.InsertTask(ctx, storage.TaskInsert{Project: "reviews", Title: "Source work", Reporter: "user", RootReason: stringValue("user-directed"), Details: stringValue("Review claim integration")}, &owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "reviews", source.ID, storage.TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, &owner); err != nil {
		t.Fatal(err)
	}
	reviews, err := s.TaskReviewChildren(ctx, "reviews", source.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("reviews %+v %v", reviews, err)
	}
	review := reviews[0]
	c, err := s.CreateConversation(ctx, "reviews", review.ID, &owner)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	activity := now.Add(-time.Minute).UnixMilli()
	report := storage.PresenceReport{Source: "peers", Sequence: 1, Agents: []storage.PresenceAgent{
		{ProjectIdent: "reviews", Origin: owner, AutoAssign: true, LastActivityAt: &activity},
		{ProjectIdent: "reviews", Origin: peer, AutoAssign: true, LastActivityAt: &activity},
	}}
	if _, err := s.ReportPresence(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	h, err := server.NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(h)
	defer host.Close()
	client, err := New(host.URL, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"id": review.ID})
	result, err := client.CallTaskTool(ctx, "reviews", c.ID, owner, "task_claim", args)
	if err != nil || !result.IsError {
		t.Fatalf("worse reviewer bypassed ready peer: %+v %v", result, err)
	}
	report.Sequence++
	report.Agents[1].Busy = true
	if _, err := s.ReportPresence(ctx, report, now); err != nil {
		t.Fatal(err)
	}
	result, err = client.CallTaskTool(ctx, "reviews", c.ID, owner, "task_claim", args)
	if err != nil || result.IsError {
		t.Fatalf("busy peer blocked fallback: %+v %v", result, err)
	}
	args, _ = json.Marshal(map[string]any{"id": review.ID, "changeset": nil})
	result, err = client.CallTaskTool(ctx, "reviews", c.ID, owner, "task_done", args)
	if err != nil || result.IsError {
		t.Fatalf("review completion failed: %+v %v", result, err)
	}
	children, err := s.TaskReviewChildren(ctx, "reviews", review.ID)
	if err != nil || len(children) != 0 {
		t.Fatalf("review spawned another review chain: %+v %v", children, err)
	}
}

func stringValue(value string) *string { return &value }
