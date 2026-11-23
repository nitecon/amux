package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestConversationFenceRejectsStaleTaskMutation(t *testing.T) {
	ctx := context.Background()
	s, c, owner := commandConversation(t)
	task, err := s.Task(ctx, "tasks", c.TaskID)
	if err != nil || task == nil {
		t.Fatalf("task: %v", err)
	}
	other := *owner
	other.InstanceID = uuid.NewString()
	if _, err = s.ResumeConversation(ctx, "tasks", c.ID, owner.InstanceID, &other); err != nil {
		t.Fatal(err)
	}
	fenced := WithConversationFence(ctx, "tasks", c.ID)
	if _, err = s.AddTaskComment(fenced, "tasks", c.TaskID, "worker", "agent", "stale", nil, owner); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("stale comment accepted: %v", err)
	}
	if _, err = s.UpdateTaskWorkflow(fenced, "tasks", c.TaskID, TaskWorkflowUpdate{Status: "done"}, owner); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("stale completion accepted: %v", err)
	}
	if _, err = s.AddTaskComment(fenced, "tasks", c.TaskID, "worker", "agent", "nil actor", nil, nil); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("nil actor passed fence: %v", err)
	}
}

func TestConversationFenceAllowsCurrentOwner(t *testing.T) {
	ctx := context.Background()
	s, c, owner := commandConversation(t)
	fenced := WithConversationFence(ctx, "tasks", c.ID)
	if _, err := s.AddTaskComment(fenced, "tasks", c.TaskID, "worker", "agent", "current", nil, owner); err != nil {
		t.Fatal(err)
	}
}
