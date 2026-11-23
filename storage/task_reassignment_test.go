package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func reassignmentFixture(t *testing.T, labels []string, now time.Time) (*Store, *Task, AgentOrigin, RegisteredTargetIdentity) {
	t.Helper()
	ctx := context.Background()
	s := taskStore(t)
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	actor := *taskActor()
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Reassign", Reporter: "fixture"}, &actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) > 0 {
		if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET labels=? WHERE id=?", `["os:windows"]`, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	target := AgentOrigin{SessionID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", InstanceID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Provider: "codex", OS: "linux"}
	registered, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: target, Generation: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, actor, registered.Target
}

func TestReassignTaskRequiresExactOwnerAndLiveCompatibleTarget(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	s, task, actor, target := reassignmentFixture(t, nil, now)
	foreign := actor
	foreign.InstanceID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	if _, err := s.ReassignTask(ctx, "tasks", task.ID, foreign, target, now); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("non-owner reassignment error=%v", err)
	}
	if _, err := s.ReassignTask(ctx, "tasks", task.ID, actor, target, now.Add(time.Duration(targetRegistrationLease)*time.Millisecond+time.Millisecond)); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("stale target reassignment error=%v", err)
	}
	if _, err := s.ReassignTask(ctx, "tasks", task.ID, actor, target, now); err != nil {
		t.Fatalf("exact owner reassignment: %v", err)
	}
}

func TestReassignTaskRejectsManagedDispatchAndOSMismatch(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	s, task, actor, target := reassignmentFixture(t, []string{"os:windows"}, now)
	if _, err := s.ReassignTask(ctx, "tasks", task.ID, actor, target, now); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("OS mismatch reassignment error=%v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET labels=? WHERE id=?", `["os:plan9"]`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReassignTask(ctx, "tasks", task.ID, actor, target, now); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("invalid stored OS label reassignment error=%v", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE task_dispatches(task_id TEXT PRIMARY KEY); INSERT INTO task_dispatches VALUES(?)`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReassignTask(ctx, "tasks", task.ID, actor, target, now); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("managed dispatch reassignment error=%v", err)
	}
}
