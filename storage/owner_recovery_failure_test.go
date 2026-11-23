package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestConversationResumeFailureRollsBackTaskOwnerAndCommandInterruption(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	owner := taskActor()
	task := makeTask(t, s, owner)
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", owner); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation(ctx, "tasks", task.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	commandID := uuid.NewString()
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, commandID, conversation.Command{Kind: "prompt", Text: "inspect"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimConversationCommand(ctx, "tasks", c.ID, commandID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_owner_handoff BEFORE UPDATE ON amux_conversations BEGIN SELECT RAISE(ABORT,'injected owner handoff failure'); END;`); err != nil {
		t.Fatal(err)
	}
	next := *owner
	next.InstanceID = uuid.NewString()
	if _, err := s.ResumeConversation(ctx, "tasks", c.ID, owner.InstanceID, &next); err == nil {
		t.Fatal("injected transfer failure was swallowed")
	}
	loaded, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || loaded.Task.OwnerOrigin == nil || *loaded.Task.OwnerOrigin != *owner {
		t.Fatalf("task partially transferred: %+v %v", loaded, err)
	}
	saved, err := s.Conversation(ctx, "tasks", c.ID)
	if err != nil || saved.Owner != *owner {
		t.Fatalf("conversation partially transferred: %+v %v", saved, err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", c.ID, 0, 100)
	if err != nil || len(commands) != 1 || commands[0].State != "claimed" {
		t.Fatalf("command interruption committed on failed handoff: %+v %v", commands, err)
	}
}
