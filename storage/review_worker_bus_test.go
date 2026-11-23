package storage

import (
	"context"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
)

func TestReviewWorkerIgnoresConversationChangesAndReconcilesOnTick(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	task := makeTask(t, s, taskActor())
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	reconciled := make(chan struct{}, 1)
	go func() { done <- s.runTaskReviewWorker(workerCtx, reconciled) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("review worker: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("review worker did not stop")
		}
	}()
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish startup reconciliation")
	}
	completedAt := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET status='done',task_type='software',done_at=?,changeset_json='["main.go"]' WHERE id=?`, completedAt, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO task_review_outbox(source_task_id,source_done_at) VALUES(?,?)", task.ID, completedAt); err != nil {
		t.Fatal(err)
	}
	s.wakeConversations()
	select {
	case <-reconciled:
		t.Fatal("conversation notification triggered review retry")
	case <-time.After(100 * time.Millisecond):
	}
	if err := s.EventBus().Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		t.Fatal("tick did not trigger review reconciliation")
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", task.ID)
	if err != nil || len(reviews) != 2 {
		var lastError string
		_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(last_error,'') FROM task_review_outbox WHERE source_task_id=?", task.ID).Scan(&lastError)
		t.Fatalf("tick did not reconcile review obligation: %+v, %v (outbox error %q)", reviews, err, lastError)
	}
}
