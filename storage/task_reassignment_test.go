package storage

import (
	"context"
	"encoding/json"
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

func TestUserReviewCommentRequeuesForLivePreviousAgent(t *testing.T) {
	ctx := context.Background()
	for _, live := range []bool{true, false} {
		t.Run(map[bool]string{true: "live prior session", false: "expired prior session"}[live], func(t *testing.T) {
			now := time.Now()
			s, task, actor, _ := reassignmentFixture(t, nil, now)
			if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{
				Status: "user_review", ReviewAction: ptr("Choose the option"), Changeset: []byte("null"),
			}, &actor); err != nil {
				t.Fatal(err)
			}
			prior := actor
			prior.InstanceID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			seen := now
			if !live {
				seen = now.Add(-5 * time.Minute)
			}
			registered, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{Origin: prior, Generation: prior.InstanceID}, seen)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddTaskComment(ctx, "tasks", task.ID, actor.SessionID, "agent", "Still waiting", nil, &actor); err != nil {
				t.Fatal(err)
			}
			detail, err := s.Task(ctx, "tasks", task.ID)
			if err != nil || detail.Task.Status != "user_review" {
				t.Fatalf("agent comment changed review state: %+v, %v", detail, err)
			}
			comment, err := s.AddTaskComment(ctx, "tasks", task.ID, "user", "user", "Use the first option", nil, nil)
			if err != nil || comment == nil {
				t.Fatalf("user reply: %+v, %v", comment, err)
			}
			detail, err = s.Task(ctx, "tasks", task.ID)
			if err != nil || detail.Task.Status != "todo" || detail.Task.ReviewAction != nil || detail.Task.StartedAt != nil {
				t.Fatalf("user reply did not requeue task: %+v, %v", detail, err)
			}
			var routing TaskRouting
			if err := json.Unmarshal(detail.Task.Routing, &routing); err != nil {
				t.Fatal(err)
			}
			if !live {
				if detail.Task.OwnerOrigin != nil || routing.Mode != "auto" {
					t.Fatalf("expired agent blocked automatic assignment: %+v", detail.Task)
				}
				return
			}
			if detail.Task.OwnerOrigin == nil || *detail.Task.OwnerOrigin != prior || routing.Mode != "session" || routing.Target == nil || *routing.Target != registered.Target {
				t.Fatalf("live agent was not preferred: %+v", detail.Task)
			}
			assignment, err := s.AdoptWorkerTask(ctx, "tasks", task.ID, &prior, nil, "")
			if err != nil || assignment == nil || assignment.Task.Status != "in_progress" {
				t.Fatalf("previous agent could not adopt reply: %+v, %v", assignment, err)
			}
		})
	}
}
