package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

// Exercise the real task trigger -> durable event -> plugin -> domain boundary,
// including a failed generation attempt after completion has already committed.
func TestReviewPluginRetriesDurableCompletionWithoutDuplicatingLegacyReviews(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := makeTask(t, s, taskActor())
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_router_review BEFORE INSERT ON tasks WHEN NEW.reporter='amux-autoreview' BEGIN SELECT RAISE(ABORT,'review integration failure'); END`); err != nil {
		t.Fatal(err)
	}
	done, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, taskActor())
	if err != nil || done.Status != "done" {
		t.Fatalf("completion must survive review failure: %+v %v", done, err)
	}
	runtime, err := router.NewRuntime(s, s.EventBus(), RouterTaskEventDefinitions(), []router.Plugin{plugins.ReviewPlugin(s)}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainRouterTaskOutbox(ctx, runtime.Registry(), 200); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT seq FROM router_events WHERE id='task.completed' AND json_extract(payload,'$.task.id')=?`, source.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	waitRouterDeliveryState(t, s, seq, "review", "retrying")
	stop()
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_reviews", int64(0))
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_router_review"); err != nil {
		t.Fatal(err)
	}
	// The existing worker remains during migration. Either controller may win;
	// the same domain uniqueness rules must converge to one pair of reviews.
	if failed, err := s.RetryTaskReviews(ctx); err != nil || failed {
		t.Fatalf("legacy recovery: %v %v", failed, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE router_deliveries SET next_attempt_at=0 WHERE event_seq=? AND plugin='review'", seq); err != nil {
		t.Fatal(err)
	}
	restarted, err := router.NewRuntime(s, s.EventBus(), RouterTaskEventDefinitions(), []router.Plugin{plugins.ReviewPlugin(s)}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stop = runRouterFixture(t, restarted)
	waitRouterDeliveryState(t, s, seq, "review", "completed")
	stop()
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_reviews", int64(2))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM task_review_outbox", int64(0))
	current, err := s.Task(ctx, "tasks", source.ID)
	if err != nil || current.Task.Status != "done" || current.Task.DoneAt == nil || *current.Task.DoneAt != *done.DoneAt {
		t.Fatalf("review retry changed completion: %+v %v", current, err)
	}
}
