package storage

import (
	"context"
	"testing"
	"time"
)

func TestAssignmentBatchFailureRollsBackClaimsAndEvents(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	a := makeTask(t, s, nil)
	b := makeTask(t, s, nil)
	setTaskCreatedAt(t, s, a, 1)
	setTaskCreatedAt(t, s, b, 2)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_assignment BEFORE UPDATE ON tasks
 WHEN NEW.status='in_progress' AND OLD.created_at=2
 BEGIN SELECT RAISE(ABORT,'injected assignment failure'); END;`); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	other := *taskActor()
	other.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	other.InstanceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	reportAssignmentPresence(t, s, now, "host", 1,
		idleAssignmentAgent("tasks", *taskActor(), now), idleAssignmentAgent("tasks", other, now))
	before, err := s.TaskEvents(ctx, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAssignments(ctx, now); err == nil {
		t.Fatal("injected failure was swallowed")
	}
	for _, task := range []*Task{a, b} {
		got, err := s.Task(ctx, "tasks", task.ID)
		if err != nil || got.Task.Status != "todo" || got.Task.OwnerOrigin != nil {
			t.Fatalf("failed tick left partial claim: %+v, %v", got, err)
		}
	}
	after, err := s.TaskEvents(ctx, EventQuery{})
	if err != nil || len(after) != len(before) {
		t.Fatalf("failed tick published durable events: before=%d after=%d err=%v", len(before), len(after), err)
	}
	var turns int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM assignment_turns WHERE project='tasks'").Scan(&turns); err != nil || turns != 0 {
		t.Fatalf("failed tick changed ordinary/review turn: count=%d err=%v", turns, err)
	}
	var actor *string
	if err := s.db.QueryRowContext(ctx, "SELECT origin_json FROM task_mutation_context WHERE id=1").Scan(&actor); err != nil || actor != nil {
		t.Fatalf("mutation context leaked: %v %v", actor, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_assignment"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileAssignments(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, task := range []*Task{a, b} {
		got, err := s.Task(ctx, "tasks", task.ID)
		if err != nil || got.Task.Status != "in_progress" {
			t.Fatalf("retry failed: %+v, %v", got, err)
		}
	}
}
