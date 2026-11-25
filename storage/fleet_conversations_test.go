package storage

import (
	"context"
	"errors"
	"testing"
	"time"
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
}
