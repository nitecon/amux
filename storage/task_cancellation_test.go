package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestCancelUnownedTaskAndReplay(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	actor := taskActor()
	wake := s.TaskEventChanges()
	response, err := s.CancelTask(ctx, "TASKS", task.ID, "  obsolete  ", json.RawMessage(`["a.go","b.go"]`), actor)
	if err != nil || response.Task == nil || response.Task.Status != "cancelled" || len(response.Task.Changeset) != 2 {
		t.Fatalf("cancel %+v, %v", response, err)
	}
	c := response.Cancellation
	if c == nil || c.TaskID != task.ID || c.ProjectIdent != "tasks" || c.Reason != "obsolete" || c.PriorStatus != "todo" || c.ReporterOrigin != *actor || c.ExecutorStopConfirmed {
		t.Fatalf("cancellation %+v", c)
	}
	wire, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil || len(fields) != 10 || fields["prior_owner_origin"] == nil || fields["executor_stop_confirmed"] == nil {
		t.Fatalf("cancellation JSON contract %s (%v)", wire, err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("commit did not wake task subscribers")
	}
	events, err := s.TaskEvents(ctx, EventQuery{TaskID: &task.ID})
	if err != nil || len(events) != 2 || events[0].Kind != "task_cancelled" || events[0].Origin == nil || *events[0].Origin != *actor {
		t.Fatalf("events %+v, %v", events, err)
	}
	var snapshot Task
	if err := json.Unmarshal(events[0].Task, &snapshot); err != nil || len(snapshot.Changeset) != 2 {
		t.Fatalf("snapshot changeset %+v, %v", snapshot, err)
	}
	replay, err := s.CancelTask(ctx, "tasks", task.ID, "obsolete", nil, actor)
	if err != nil || replay.Task.UpdatedAt != response.Task.UpdatedAt || replay.Cancellation.CancelledAt != c.CancelledAt {
		t.Fatalf("replay %+v, %v", replay, err)
	}
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "different", json.RawMessage(`null`), actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("different replay reason: %v", err)
	}
	other := *actor
	other.Provider = "claude"
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "obsolete", json.RawMessage(`null`), &other); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("different full reporter origin: %v", err)
	}
	events, err = s.TaskEvents(ctx, EventQuery{TaskID: &task.ID})
	if err != nil || len(events) != 2 {
		t.Fatalf("replay added lifecycle events: %+v, %v", events, err)
	}
}

func TestCancelClaimedTaskPreservesClaimProvenance(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	actor := taskActor()
	claimed, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", actor)
	if err != nil {
		t.Fatal(err)
	}
	other := *actor
	other.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "other", json.RawMessage(`null`), &other); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("foreign claimant cancelled: %v", err)
	}
	response, err := s.CancelTask(ctx, "tasks", task.ID, "stopped", json.RawMessage(`null`), actor)
	if err != nil || response.Task.OwnerOrigin == nil || *response.Task.OwnerOrigin != *actor || response.Task.OwnerAgentID == nil || response.Task.StartedAt == nil || *response.Task.StartedAt == 0 {
		t.Fatalf("cancel lost owner provenance: %+v, %v", response, err)
	}
	if response.Cancellation.PriorStatus != "in_progress" || response.Cancellation.PriorStartedAt == nil || *response.Cancellation.PriorStartedAt != *claimed.StartedAt || response.Cancellation.PriorOwnerOrigin == nil || *response.Cancellation.PriorOwnerOrigin != *actor {
		t.Fatalf("prior claim %+v", response.Cancellation)
	}
}

func TestCancelRejectsUnboundDoneReviewAndDispatchTasks(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(*testing.T, *Store, *Task)
		want  error
	}{
		{"unbound owned", func(t *testing.T, s *Store, task *Task) {
			if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='in_progress',owner_agent_id='legacy' WHERE id=?", task.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrTaskOwnership},
		{"done", func(t *testing.T, s *Store, task *Task) {
			if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`null`)}, taskActor()); err != nil {
				t.Fatal(err)
			}
		}, ErrTaskConflict},
		{"user review", func(t *testing.T, s *Store, task *Task) {
			if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "user_review", ReviewAction: ptr("decision"), Changeset: json.RawMessage(`null`)}, taskActor()); err != nil {
				t.Fatal(err)
			}
		}, ErrTaskConflict},
		{"non-normal", func(t *testing.T, s *Store, task *Task) {
			if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET kind='dispatch' WHERE id=?", task.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrTaskUnsupported},
		{"managed dispatch", func(t *testing.T, s *Store, task *Task) {
			if _, err := s.db.ExecContext(ctx, `CREATE TABLE task_dispatches(task_id TEXT PRIMARY KEY,project_ident TEXT NOT NULL,document_json TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, "INSERT INTO task_dispatches VALUES(?,?,?)", task.ID, "tasks", "{}"); err != nil {
				t.Fatal(err)
			}
		}, ErrTaskUnsupported},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := taskStore(t)
			task := makeTask(t, s, nil)
			tc.setup(t, s, task)
			if _, err := s.CancelTask(ctx, "tasks", task.ID, "stop", json.RawMessage(`null`), taskActor()); !errors.Is(err, tc.want) {
				t.Fatalf("cancel error %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("cancelled only done", func(t *testing.T) {
		s := taskStore(t)
		task := makeTask(t, s, nil)
		actor := taskActor()
		if _, err := s.CancelTask(ctx, "tasks", task.ID, "stop", json.RawMessage(`null`), actor); err != nil {
			t.Fatal(err)
		}
		done, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`null`)}, actor)
		if err != nil || done.Status != "done" {
			t.Fatalf("terminal completion %+v, %v", done, err)
		}
	})
}

func TestCancelRequiresReasonOriginAndExplicitValidChangeset(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	actor := taskActor()
	for _, reason := range []string{"  ", strings.Repeat("x", 16385)} {
		if _, err := s.CancelTask(ctx, "tasks", task.ID, reason, json.RawMessage(`null`), actor); !errors.Is(err, ErrTaskCancellationInput) {
			t.Fatalf("reason validation: %v", err)
		}
	}
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "stop", json.RawMessage(`[]`), actor); !errors.Is(err, ErrTaskCancellationInput) {
		t.Fatalf("invalid changeset: %v", err)
	}
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "stop", nil, actor); !errors.Is(err, ErrTaskChangeset) {
		t.Fatalf("missing changeset: %v", err)
	}
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "stop", json.RawMessage(`null`), nil); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("missing actor: %v", err)
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || detail.Task.Status != "todo" {
		t.Fatalf("failed cancellation moved task: %+v, %v", detail, err)
	}
}

func TestCancelSQLFailureRollsBackAllWrites(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	actor := taskActor()
	wake := s.TaskEventChanges()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_cancel_record BEFORE INSERT ON task_cancellations BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "stop", json.RawMessage(`null`), actor); err == nil {
		t.Fatal("expected injected SQL failure")
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || detail.Task.Status != "todo" {
		t.Fatalf("task after rollback %+v %v", detail, err)
	}
	if cancellation, err := s.TaskCancellation(ctx, "tasks", task.ID); err != nil || cancellation != nil {
		t.Fatalf("record after rollback %+v %v", cancellation, err)
	}
	var events int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events WHERE kind='task_cancelled'").Scan(&events); err != nil || events != 0 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	select {
	case <-wake:
		t.Fatal("rollback woke subscribers")
	default:
	}
}

func TestTaskCancellationMigrationAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancel.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.InsertProject(context.Background(), fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	task, err := s.InsertTask(context.Background(), TaskInsert{Project: "tasks", Title: "Work", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.CancelTask(context.Background(), "tasks", task.ID, "stop", json.RawMessage(`null`), taskActor())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.TaskCancellation(context.Background(), "tasks", task.ID)
	if err != nil || got == nil || got.CancelledAt != response.Cancellation.CancelledAt {
		t.Fatalf("reopened cancellation %+v, %v", got, err)
	}
	var version int
	if err = s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM amux_go_migrations WHERE version=14").Scan(&version); err != nil || version != 1 {
		t.Fatalf("migration 14 count %d, %v", version, err)
	}
}

func TestCancelAgainAfterCompleteAndReopenReplacesCancellation(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, nil)
	actor := taskActor()
	if _, err := s.CancelTask(ctx, "tasks", task.ID, "reason A", json.RawMessage(`null`), actor); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"done", "todo"} {
		if _, err := s.ReorderTasks(ctx, "tasks", status, []string{task.ID}, nil, TaskTransitionPolicy{HumanRequest: true}); err != nil {
			t.Fatalf("reopen through %s: %v", status, err)
		}
	}
	response, err := s.CancelTask(ctx, "tasks", task.ID, "reason B", json.RawMessage(`null`), actor)
	if err != nil || response.Task.Status != "cancelled" || response.Cancellation.Reason != "reason B" {
		t.Fatalf("second cancellation %+v, %v", response, err)
	}
	var rows int
	var reason string
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),MAX(reason) FROM task_cancellations WHERE task_id=?", task.ID).Scan(&rows, &reason); err != nil || rows != 1 || reason != "reason B" {
		t.Fatalf("cancellation rows=%d reason=%q err=%v", rows, reason, err)
	}
}

func TestCancelSupportsAntigravityOrigin(t *testing.T) {
	s := taskStore(t)
	task := makeTask(t, s, nil)
	actor := taskActor()
	actor.Provider = "antigravity"
	response, err := s.CancelTask(context.Background(), "tasks", task.ID, "stop", json.RawMessage(`null`), actor)
	if err != nil || response.Cancellation.ReporterOrigin.Provider != "antigravity" {
		t.Fatalf("antigravity cancellation %+v, %v", response, err)
	}
}
