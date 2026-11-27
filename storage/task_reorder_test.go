package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestReorderTasksPreservesSameColumnReservationAndRollsBackBatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	actor := apiStorageActor()
	goal := "focused"
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "reserved", Reporter: "user", Goal: &goal}, actor)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an existing reservation that has been returned to the queue.
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='todo',started_at=NULL,rank=17 WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReorderTasks(ctx, "tasks", "todo", task.ID, []string{task.ID}, nil, TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Task.Rank != 0 || after.Task.OwnerOrigin == nil || after.Task.OwnerOrigin.SessionID != before.Task.OwnerOrigin.SessionID || after.Task.StartedAt != before.Task.StartedAt {
		t.Fatalf("same-column reorder changed reservation/lifecycle: before=%+v after=%+v", before.Task, after.Task)
	}

	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "second", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReorderTasks(ctx, "tasks", "in_progress", second.ID, []string{second.ID, "missing"}, nil, TaskTransitionPolicy{HumanRequest: true}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing ID error = %v", err)
	}
	var status string
	var rank int64
	if err := s.db.QueryRowContext(ctx, "SELECT status,rank FROM tasks WHERE id=?", second.ID).Scan(&status, &rank); err != nil {
		t.Fatal(err)
	}
	var eventsAfter int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if status != "todo" || rank != second.Rank || eventsAfter != eventsBefore {
		t.Fatalf("failed batch leaked effects: status=%s rank=%d events=%d (before %d)", status, rank, eventsAfter, eventsBefore)
	}
}

func TestReorderTasksGoalPolicyRollsBackEarlierClaim(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	goal := "chosen"
	first, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "first", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "goal task", Reporter: "user", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var eventsBefore int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReorderTasks(ctx, "tasks", "in_progress", first.ID, []string{first.ID, second.ID}, apiStorageActor(), TaskTransitionPolicy{CheckGoal: true, SelectedGoal: "other"})
	if !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("goal policy error = %v", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		detail, err := s.Task(ctx, "tasks", id)
		if err != nil || detail.Task.Status != "todo" || detail.Task.OwnerOrigin != nil {
			t.Fatalf("task %s leaked claim: %+v, %v", id, detail, err)
		}
	}
	var eventsAfter int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("failed claim emitted events: before=%d after=%d", eventsBefore, eventsAfter)
	}
}

func apiStorageActor() *AgentOrigin {
	return &AgentOrigin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
}

func TestReorderCrossingEventCapturesFinalRank(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	first, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "first", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "second", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", first.ID, TaskWorkflowUpdate{Status: "done", Policy: TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReorderTasks(ctx, "tasks", "done", second.ID, []string{first.ID, second.ID}, nil, TaskTransitionPolicy{HumanRequest: true}); err != nil {
		t.Fatal(err)
	}
	var rank int64
	err = s.db.QueryRowContext(ctx, `SELECT json_extract(task_json,'$.rank') FROM task_events WHERE kind='task_completed' AND json_extract(task_json,'$.id')=? ORDER BY id DESC LIMIT 1`, second.ID).Scan(&rank)
	if err != nil {
		t.Fatal(err)
	}
	if rank != 1 {
		t.Fatalf("completion event rank = %d, want final rank 1", rank)
	}
}

func TestReorderWorkflowFailuresRollBackRanksAndEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	first, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "first", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "second", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	countEvents := func() int {
		t.Helper()
		var count int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	assertTask := func(id, status string, rank int64) {
		t.Helper()
		var gotStatus string
		var gotRank int64
		if err := s.db.QueryRowContext(ctx, "SELECT status,rank FROM tasks WHERE id=?", id).Scan(&gotStatus, &gotRank); err != nil {
			t.Fatal(err)
		}
		if gotStatus != status || gotRank != rank {
			t.Fatalf("task %s state=(%s,%d), want (%s,%d)", id, gotStatus, gotRank, status, rank)
		}
	}

	// An agent's cross-column completion needs the changeset-aware PATCH path.
	events := countEvents()
	if _, err := s.ReorderTasks(ctx, "tasks", "done", first.ID, []string{first.ID}, apiStorageActor(), TaskTransitionPolicy{}); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "changeset") {
		t.Fatalf("agent completion crossing error = %v", err)
	}
	assertTask(first.ID, "todo", first.Rank)
	assertTask(second.ID, "todo", second.Rank)
	if countEvents() != events {
		t.Fatal("rejected agent completion emitted durable events")
	}

	// A same-column review rank change must roll back if another entry attempts
	// to enter review without a review_action.
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='user_review',review_action='needs review',rank=22 WHERE id=?", first.ID); err != nil {
		t.Fatal(err)
	}
	events = countEvents()
	if _, err := s.ReorderTasks(ctx, "tasks", "user_review", second.ID, []string{first.ID, second.ID}, nil, TaskTransitionPolicy{HumanRequest: true}); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "review_action") {
		t.Fatalf("review entry error = %v", err)
	}
	assertTask(first.ID, "user_review", 22)
	assertTask(second.ID, "todo", second.Rank)
	if countEvents() != events {
		t.Fatal("rejected review reorder emitted durable events")
	}

	// A stale legacy owner cannot use an agent crossing to release a task. The
	// rank write made before workflow validation must be part of the rollback.
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='in_progress',owner_agent_id='stale',owner_origin_json=NULL,started_at=1,rank=31 WHERE id=?", second.ID); err != nil {
		t.Fatal(err)
	}
	events = countEvents()
	if _, err := s.ReorderTasks(ctx, "tasks", "todo", second.ID, []string{second.ID}, apiStorageActor(), TaskTransitionPolicy{}); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("stale owner error = %v", err)
	}
	assertTask(second.ID, "in_progress", 31)
	if countEvents() != events {
		t.Fatal("stale owner rejection emitted durable events")
	}
}

func TestReorderTasksRejectsConcurrentDestinationStatusChangeAtomically(t *testing.T) {
	for _, destination := range []string{"todo", "in_progress"} {
		t.Run(destination, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
				t.Fatal(err)
			}
			dragged, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "dragged", Reporter: "user"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			resident, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "destination row", Reporter: "user"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if destination != "todo" {
				if _, err := s.UpdateTaskWorkflow(ctx, "tasks", resident.ID, TaskWorkflowUpdate{Status: destination, Policy: TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			// The board renders this order, then another request completes the
			// destination row before the drag request reaches storage.
			order := []string{dragged.ID, resident.ID}
			if _, err := s.UpdateTaskWorkflow(ctx, "tasks", resident.ID, TaskWorkflowUpdate{Status: "done", Policy: TaskTransitionPolicy{HumanRequest: true}}, nil); err != nil {
				t.Fatal(err)
			}
			before, err := s.Tasks(ctx, "tasks", nil, true)
			if err != nil {
				t.Fatal(err)
			}
			var eventsBefore int
			if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&eventsBefore); err != nil {
				t.Fatal(err)
			}
			_, err = s.ReorderTasks(ctx, "tasks", destination, dragged.ID, order, nil, TaskTransitionPolicy{HumanRequest: true})
			if !errors.Is(err, ErrTaskConflict) {
				t.Fatalf("stale reorder error = %v, want conflict", err)
			}
			after, err := s.Tasks(ctx, "tasks", nil, true)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("stale reorder changed tasks: before=%+v after=%+v", before, after)
			}
			var eventsAfter int
			if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_events").Scan(&eventsAfter); err != nil {
				t.Fatal(err)
			}
			if eventsAfter != eventsBefore {
				t.Fatalf("stale reorder leaked events: before=%d after=%d", eventsBefore, eventsAfter)
			}
		})
	}
}
