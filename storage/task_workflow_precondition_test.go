package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func makeUserReviewTask(t *testing.T, s *Store) *Task {
	t.Helper()
	ctx := context.Background()
	owner := taskActor()
	task := makeTask(t, s, owner)
	updated, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{
		Status: "user_review", ReviewAction: ptr("Choose the next step"), Changeset: json.RawMessage(`null`),
	}, owner)
	if err != nil || updated.Status != "user_review" {
		t.Fatalf("move task to user review: %+v %v", updated, err)
	}
	return updated
}

func TestTaskWorkflowExpectedStatusRejectsStaleResumeAtomically(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeUserReviewTask(t, s)
	closed, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage(`null`), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil)
	if err != nil || closed.Status != "done" {
		t.Fatalf("human close failed: %+v %v", closed, err)
	}
	var eventsBefore, outboxBefore, commentsBefore int64
	for query, target := range map[string]*int64{
		"SELECT COUNT(*) FROM task_events":                                   &eventsBefore,
		"SELECT COUNT(*) FROM router_task_outbox":                            &outboxBefore,
		"SELECT COUNT(*) FROM task_comments WHERE task_id='" + task.ID + "'": &commentsBefore,
	} {
		if err := s.db.QueryRowContext(ctx, query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	title := "must not be applied"
	expected := "user_review"
	_, err = s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{
		Status: "todo", ExpectedStatus: &expected,
		TaskMetadataUpdate: TaskMetadataUpdate{Title: &title},
		Changeset:          json.RawMessage(`null`),
		Policy:             TaskTransitionPolicy{HumanRequest: true},
	}, nil)
	if !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("stale conditional resume error=%v, want ErrTaskConflict", err)
	}
	current, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || current.Task.Status != "done" || current.Task.Title == title {
		t.Fatalf("stale resume changed task: %+v %v", current, err)
	}
	var eventsAfter, outboxAfter, commentsAfter int64
	for query, target := range map[string]*int64{
		"SELECT COUNT(*) FROM task_events":                                   &eventsAfter,
		"SELECT COUNT(*) FROM router_task_outbox":                            &outboxAfter,
		"SELECT COUNT(*) FROM task_comments WHERE task_id='" + task.ID + "'": &commentsAfter,
	} {
		if err := s.db.QueryRowContext(ctx, query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if eventsAfter != eventsBefore || outboxAfter != outboxBefore || commentsAfter != commentsBefore {
		t.Fatalf("stale resume emitted side effects: events %d->%d outbox %d->%d comments %d->%d", eventsBefore, eventsAfter, outboxBefore, outboxAfter, commentsBefore, commentsAfter)
	}
	var confirmations int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_confirmations WHERE task_id=?", task.ID).Scan(&confirmations); err != nil || confirmations != 0 {
		t.Fatalf("stale resume staged confirmation: count=%d err=%v", confirmations, err)
	}
}

func TestTaskWorkflowExpectedStatusMatchAndUnconditionalReopen(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	review := makeUserReviewTask(t, s)
	expected := "user_review"
	resumed, err := s.UpdateTaskWorkflow(ctx, "tasks", review.ID, TaskWorkflowUpdate{
		Status: "todo", ExpectedStatus: &expected, Changeset: json.RawMessage(`null`),
		Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil)
	if err != nil || resumed.Status != "todo" {
		t.Fatalf("matching conditional resume: %+v %v", resumed, err)
	}

	done := makeTask(t, s, nil)
	completed, err := s.UpdateTaskWorkflow(ctx, "tasks", done.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage(`null`), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil)
	if err != nil || completed.Status != "done" {
		t.Fatalf("explicit completion: %+v %v", completed, err)
	}
	reopened, err := s.UpdateTaskWorkflow(ctx, "tasks", done.ID, TaskWorkflowUpdate{
		Status: "todo", Changeset: json.RawMessage(`null`), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil)
	if err != nil || reopened.Status != "todo" {
		t.Fatalf("unconditional explicit reopen no longer works: %+v %v", reopened, err)
	}
}

func TestTaskWorkflowExpectedStatusMismatchPreventsGuidedPreview(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, taskActor())
	expected := "user_review"
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{
		Status: "done", ExpectedStatus: &expected,
	}, taskActor()); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("mismatched precondition returned %v; want conflict before guided question", err)
	}
	var confirmations int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_confirmations WHERE task_id=?", task.ID).Scan(&confirmations); err != nil || confirmations != 0 {
		t.Fatalf("mismatched precondition staged confirmation: count=%d err=%v", confirmations, err)
	}
	current, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || current.Task.Status != "todo" {
		t.Fatalf("mismatched precondition moved task: %+v %v", current, err)
	}
}

func TestTaskWorkflowExpectedStatusValidation(t *testing.T) {
	for _, status := range []string{"todo", "in_progress", "user_review", "done", "cancelled"} {
		if err := validateExpectedTaskStatus(&status); err != nil {
			t.Errorf("canonical status %q rejected: %v", status, err)
		}
	}
	invalid := "not-a-task-status"
	if err := validateExpectedTaskStatus(&invalid); !errors.Is(err, ErrTaskMetadataInput) {
		t.Fatalf("unknown expected status error=%v, want ErrTaskMetadataInput", err)
	}
	if _, err := taskStore(t).UpdateTaskWorkflow(context.Background(), "tasks", "missing", TaskWorkflowUpdate{ExpectedStatus: &invalid}, nil); !errors.Is(err, ErrTaskMetadataInput) {
		t.Fatalf("invalid expected status error=%v, want ErrTaskMetadataInput", err)
	}
}
