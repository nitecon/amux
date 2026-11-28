package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestFleetConversationBindsDefaultChatToAuthenticatedAgent(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	agent := *taskActor()
	now := time.Now().UTC()
	if _, err := s.OpenFleetConversation(ctx, "tasks", agent.SessionID, now); !errors.Is(err, ErrFleetAgentUnavailable) {
		t.Fatalf("unknown agent returned %v", err)
	}
	host, principal := "linux-node", "enrolled-node"
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{
		Source: "fixture-pool", Sequence: 1, Hostname: &host,
		Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: agent}},
	}, now, principal); err != nil || !accepted {
		t.Fatalf("presence accepted %v, %v", accepted, err)
	}
	opened, err := s.OpenFleetConversation(ctx, "tasks", agent.SessionID, now)
	if err != nil || opened.Hostname != host || opened.Owner != agent || opened.TaskID != "" {
		t.Fatalf("opened %+v, %v", opened, err)
	}
	second, err := s.OpenFleetConversation(ctx, "tasks", agent.SessionID, now.Add(time.Hour))
	if err != nil || second.ID != opened.ID {
		t.Fatalf("missing durable default: %+v, %v", second, err)
	}
	owned, err := s.FleetConversations(ctx, principal)
	if err != nil || len(owned) != 1 || owned[0].ID != opened.ID {
		t.Fatalf("owning peer listed %+v, %v", owned, err)
	}
	other, err := s.FleetConversations(ctx, "other-node")
	if err != nil || len(other) != 0 {
		t.Fatalf("other peer listed %+v, %v", other, err)
	}
	if allowed, err := s.FleetConversationWriteAllowed(ctx, opened.ID, "other-node"); err != nil || allowed {
		t.Fatalf("different peer can write: %v, %v", allowed, err)
	}
	if err := s.CloseFleetConversation(ctx, "tasks", opened.ID, "other-node", false); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("different peer closed chat: %v", err)
	}
	if err := s.CloseFleetConversation(ctx, "wrong-project", opened.ID, principal, false); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("wrong project closed chat: %v", err)
	}
	if _, err := s.BindConversationSession(ctx, "tasks", opened.ID, "missing-rollout", &agent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendConversationEvent(ctx, "tasks", opened.ID, uuid.NewString(), conversation.Event{Kind: "local_error", SessionID: "missing-rollout", Text: "no rollout found"}, &agent); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseFleetConversation(ctx, "tasks", opened.ID, principal, false); err != nil {
		t.Fatal(err)
	}
	owned, err = s.FleetConversations(ctx, principal)
	if err != nil || len(owned) != 0 {
		t.Fatalf("closed chat still scheduled: %+v, %v", owned, err)
	}
	events, err := s.ConversationEvents(ctx, "tasks", opened.ID, 0, 100)
	if err != nil || len(events) != 1 || events[0].Event.Text != "no rollout found" {
		t.Fatalf("lost transcript: %+v, %v", events, err)
	}
	if _, err := s.ResumeConversation(ctx, "tasks", opened.ID, agent.InstanceID, &agent); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("closed chat resumed: %v", err)
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", opened.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "retry"}); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("closed chat accepted prompt: %v", err)
	}
	fresh, err := s.OpenFleetConversation(ctx, "tasks", agent.SessionID, now)
	if err != nil || fresh.ID == opened.ID || fresh.NativeSessionID != "" {
		t.Fatalf("reopen did not create fresh chat: %+v, %v", fresh, err)
	}
	if err := s.CloseFleetConversation(ctx, "tasks", fresh.ID, "admin-node", true); err != nil {
		t.Fatalf("administrator close: %v", err)
	}
}
