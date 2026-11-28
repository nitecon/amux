package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/identity/agent"
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

func TestFleetConversationFailoverPrefersOtherProvider(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := time.Now().UTC()
	broken := *taskActor()
	sameProvider := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	otherProvider := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	busy := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	standalone := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	report := func(source, principal string, seen time.Time, origins ...agent.Origin) {
		host := source + "-host"
		agents := make([]PresenceAgent, 0, len(origins))
		for _, origin := range origins {
			agents = append(agents, PresenceAgent{ProjectIdent: "tasks", Origin: origin})
		}
		if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: source, Sequence: 1, Hostname: &host, Agents: agents}, seen, principal); err != nil || !accepted {
			t.Fatalf("presence accepted %v, %v", accepted, err)
		}
	}
	report("broken", "broken-node", now, broken)
	chat, err := s.OpenFleetConversation(ctx, "tasks", broken.SessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailoverFleetConversation(ctx, "tasks", chat.ID, "broken-node", false, FleetFailoverExpectation{}, now); !errors.Is(err, ErrFleetAgentUnavailable) {
		t.Fatalf("failover without candidates returned %v", err)
	}
	report("same/slot", "same-node", now, sameProvider)
	report("other/slot", "other-node", now, otherProvider)
	// The most recently seen agent already has its own chat and is skipped.
	report("busy/slot", "busy-node", now.Add(time.Second), busy)
	// A standalone run is not started by a fleet runner and is skipped.
	report("worker:"+uuid.NewString(), "standalone-node", now.Add(2*time.Second), standalone)
	if _, err := s.OpenFleetConversation(ctx, "tasks", busy.SessionID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSession(ctx, "tasks", chat.ID, "missing-rollout", &broken); err != nil {
		t.Fatal(err)
	}
	prompt, err := s.EnqueueConversationCommand(ctx, "tasks", chat.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailoverFleetConversation(ctx, "tasks", chat.ID, "other-node", false, FleetFailoverExpectation{}, now); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("foreign node moved chat: %v", err)
	}
	moved, err := s.FailoverFleetConversation(ctx, "tasks", chat.ID, "broken-node", false, FleetFailoverExpectation{}, now)
	if err != nil || moved.ID != chat.ID || moved.Owner != otherProvider || moved.Provider != "claude" || moved.NativeSessionID != "" || moved.Hostname != "other/slot-host" {
		t.Fatalf("moved %+v, %v", moved, err)
	}
	if owned, err := s.FleetConversations(ctx, "broken-node"); err != nil || len(owned) != 0 {
		t.Fatalf("previous node still runs chat: %+v, %v", owned, err)
	}
	if owned, err := s.FleetConversations(ctx, "other-node"); err != nil || len(owned) != 1 || owned[0].ID != chat.ID || owned[0].Owner != otherProvider {
		t.Fatalf("new node listed %+v, %v", owned, err)
	}
	if reopened, err := s.OpenFleetConversation(ctx, "tasks", otherProvider.SessionID, now); err != nil || reopened.ID != chat.ID {
		t.Fatalf("new agent default chat %+v, %v", reopened, err)
	}
	if fresh, err := s.OpenFleetConversation(ctx, "tasks", broken.SessionID, now); err != nil || fresh.ID == chat.ID {
		t.Fatalf("previous agent kept moved chat %+v, %v", fresh, err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", chat.ID, 0, 10)
	if err != nil || len(commands) != 1 || commands[0].ID != prompt.ID || commands[0].State != "queued" || commands[0].TargetInstanceID != otherProvider.InstanceID {
		t.Fatalf("queued prompt did not follow chat: %+v, %v", commands, err)
	}
	events, err := s.ConversationEvents(ctx, "tasks", chat.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Event.Status != "provider_failover" {
		t.Fatalf("missing failover event: %+v, %v", events, err)
	}
	if _, err := s.BindConversationSession(ctx, "tasks", chat.ID, "fresh-thread", &otherProvider); err != nil {
		t.Fatalf("new agent cannot bind fresh thread: %v", err)
	}
}

func TestFleetConversationFailoverRejectsStaleExpectation(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	now := time.Now().UTC()
	failed := *taskActor()
	healthy := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	spare := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "claude", OS: "linux"}
	host := "node-host"
	// Both agents share one enrolled node, so ownership alone cannot fence.
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, PresenceReport{Source: "pool/a", Sequence: 1, Hostname: &host,
		Agents: []PresenceAgent{{ProjectIdent: "tasks", Origin: failed}, {ProjectIdent: "tasks", Origin: healthy}, {ProjectIdent: "tasks", Origin: spare}}}, now, "node"); err != nil || !accepted {
		t.Fatalf("presence accepted %v, %v", accepted, err)
	}
	chat, err := s.OpenFleetConversation(ctx, "tasks", failed.SessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSession(ctx, "tasks", chat.ID, "missing-rollout", &failed); err != nil {
		t.Fatal(err)
	}
	stale := FleetFailoverExpectation{SessionID: failed.SessionID, InstanceID: failed.InstanceID, NativeSessionID: "missing-rollout"}
	// An administrator moves the chat first and the new agent binds a thread.
	moved, err := s.FailoverFleetConversation(ctx, "tasks", chat.ID, "admin-node", true, FleetFailoverExpectation{}, now)
	if err != nil {
		t.Fatal(err)
	}
	target := moved.Owner
	if _, err := s.BindConversationSession(ctx, "tasks", chat.ID, "healthy-thread", &target); err != nil {
		t.Fatal(err)
	}
	prompt, err := s.EnqueueConversationCommand(ctx, "tasks", chat.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	// The failed agent's delayed automatic request must not move it back.
	if _, err := s.FailoverFleetConversation(ctx, "tasks", chat.ID, "node", false, stale, now); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("stale failover returned %v", err)
	}
	current, err := s.Conversation(ctx, "tasks", chat.ID)
	if err != nil || current.Owner != target || current.NativeSessionID != "healthy-thread" {
		t.Fatalf("stale failover changed chat %+v, %v", current, err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", chat.ID, 0, 10)
	if err != nil || len(commands) != 1 || commands[0].ID != prompt.ID || commands[0].State != "queued" {
		t.Fatalf("stale failover changed commands %+v, %v", commands, err)
	}
	// A resumed successor keeps the session and thread but not the instance;
	// the old execution's delayed request must not displace it.
	successor := target
	successor.InstanceID = uuid.NewString()
	if _, err := s.ResumeConversation(ctx, "tasks", chat.ID, target.InstanceID, &successor); err != nil {
		t.Fatal(err)
	}
	old := FleetFailoverExpectation{SessionID: target.SessionID, InstanceID: target.InstanceID, NativeSessionID: "healthy-thread"}
	if _, err := s.FailoverFleetConversation(ctx, "tasks", chat.ID, "node", false, old, now); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("stale execution failover returned %v", err)
	}
	current, err = s.Conversation(ctx, "tasks", chat.ID)
	if err != nil || current.Owner != successor || current.NativeSessionID != "healthy-thread" {
		t.Fatalf("stale execution failover changed chat %+v, %v", current, err)
	}
	commands, err = s.ConversationCommands(ctx, "tasks", chat.ID, 0, 10)
	if err != nil || len(commands) != 1 || commands[0].ID != prompt.ID || commands[0].State != "queued" {
		t.Fatalf("stale execution failover changed commands %+v, %v", commands, err)
	}
	// The expectation matching the current thread still fails over.
	if _, err := s.FailoverFleetConversation(ctx, "tasks", chat.ID, "node", false, FleetFailoverExpectation{SessionID: successor.SessionID, InstanceID: successor.InstanceID, NativeSessionID: "healthy-thread"}, now); err != nil {
		t.Fatalf("current failover returned %v", err)
	}
}
