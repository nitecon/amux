package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
)

func completeSoftwareTask(t *testing.T, s *Store) *Task {
	t.Helper()
	ctx := context.Background()
	actor := taskActor()
	source := makeTask(t, s, actor)
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done"}, actor); !errors.Is(err, ErrTaskGuidedQuestion) {
		t.Fatalf("completion did not request mutation confirmation: %v", err)
	}
	confirmed, err := s.ConfirmTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowConfirmation{TaskType: ptr("software")}, actor)
	if err != nil || !confirmed.NeedsChangeset {
		t.Fatalf("confirm software mutation: %+v %v", confirmed, err)
	}
	completed, err := s.ConfirmTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowConfirmation{Changeset: []byte(`["main.go"]`)}, actor)
	if err != nil || completed.Task == nil || completed.Task.Status != "done" {
		t.Fatalf("complete software task: %+v %v", completed, err)
	}
	return completed.Task
}

func reviewCutoverRuntime(t *testing.T, s *Store) *fism.Runtime {
	t.Helper()
	runtime, err := fism.NewRuntime(s, s.EventBus(), RouterTaskEventDefinitions(), []fism.Controller{controllers.ReviewController(s)}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func assertReviewCutoverCount(t *testing.T, s *Store, query string, want int64, args ...any) {
	t.Helper()
	var got int64
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q returned %d, want %d", query, got, want)
	}
}

func TestRouterReviewCutoverDefersCompletionToReviewPlugin(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	s.UseRouterReviews()
	source := completeSoftwareTask(t, s)
	if children, err := s.TaskReviewChildren(ctx, "tasks", source.ID); err != nil || len(children) != 0 {
		t.Fatalf("router mode synchronously created review children: %+v %v", children, err)
	}
	assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", 0, source.ID)

	runtime := reviewCutoverRuntime(t, s)
	if drained, err := s.DrainRouterTaskOutbox(ctx, runtime.Registry(), 200); err != nil || drained == 0 {
		t.Fatalf("drain completion event: count=%d err=%v", drained, err)
	}
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT seq FROM router_events WHERE id='task.completed' AND json_extract(payload,'$.task.id')=?`, source.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	waitRouterDeliveryState(t, s, seq, "review", "completed")
	stop()
	if reviews, err := s.TaskReviewChildren(ctx, "tasks", source.ID); err != nil || len(reviews) != 2 {
		t.Fatalf("review plugin did not create exactly one pair: %+v %v", reviews, err)
	}
}

func TestPublishLegacyReviewChangesReopensAndRollsBackBeforeRetry(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	source := routerReviewSource(t, s)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO task_review_outbox(source_task_id,source_done_at) VALUES(?,?)", source.ID, *source.DoneAt); err != nil {
		t.Fatal(err)
	}
	var dbPath string
	if err := s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, filepath.Clean(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	runtime := reviewCutoverRuntime(t, reopened)
	if _, err := reopened.db.ExecContext(ctx, `CREATE TRIGGER reject_review_publication BEFORE INSERT ON router_events
		WHEN NEW.id='task.completed' BEGIN SELECT RAISE(ABORT,'fixture delivery rejection'); END`); err != nil {
		t.Fatal(err)
	}
	if _, count, err := reopened.PublishLegacyReviewChanges(ctx, runtime.Registry(), 200); err == nil || count != 0 {
		t.Fatalf("failed bridge append count=%d err=%v", count, err)
	}
	assertReviewCutoverCount(t, reopened, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", 1, source.ID)
	assertReviewCutoverCount(t, reopened, "SELECT COUNT(*) FROM router_events WHERE id='task.completed' AND source='tasks'", 0)
	if _, err := reopened.db.ExecContext(ctx, "DROP TRIGGER reject_review_publication"); err != nil {
		t.Fatal(err)
	}
	changes, count, err := reopened.PublishLegacyReviewChanges(ctx, runtime.Registry(), 200)
	if err != nil || count != 1 || len(changes) != 1 {
		t.Fatalf("bridge retry count=%d changes=%d err=%v", count, len(changes), err)
	}
	change := changes[0]
	uid := change.Event.UID
	assertReviewCutoverCount(t, reopened, "SELECT COUNT(*) FROM router_deliveries WHERE event_seq=?", 0, change.Event.Seq)
	assertReviewCutoverCount(t, reopened, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", 1, source.ID)
	// Changing task metadata keeps the same completion revision. A fresh
	// observation uses the new values and preserves the broadcast sequence.
	if _, err := reopened.db.ExecContext(ctx, "UPDATE tasks SET title='Revised' WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	restarted, _, err := reopened.PublishLegacyReviewChanges(ctx, runtime.Registry(), 200)
	if err != nil || len(restarted) != 1 || restarted[0].Event.Seq != change.Event.Seq ||
		string(restarted[0].Event.Payload) == string(change.Event.Payload) {
		t.Fatalf("fresh completion observation=%+v err=%v", restarted, err)
	}
	change = restarted[0]
	outcomes, err := runtime.ReconcileEvent(ctx, change.Event)
	if err != nil || len(outcomes) != 1 || outcomes[0].State != "completed" {
		t.Fatalf("review pass outcomes=%+v err=%v", outcomes, err)
	}
	if err := reopened.SettleLegacyReviewChange(ctx, change, outcomes); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.db.ExecContext(ctx, "INSERT INTO task_review_outbox(source_task_id,source_done_at) VALUES(?,?)", source.ID, *source.DoneAt); err != nil {
		t.Fatal(err)
	}
	if changes, count, err := reopened.PublishLegacyReviewChanges(ctx, runtime.Registry(), 200); err != nil || count != 1 || len(changes) != 0 {
		t.Fatalf("replayed bridge count=%d err=%v", count, err)
	}
	var replayUID string
	if err := reopened.db.QueryRowContext(ctx, "SELECT uid FROM router_events WHERE id='task.completed' AND source='tasks'").Scan(&replayUID); err != nil {
		t.Fatal(err)
	}
	if uid != replayUID {
		t.Fatalf("bridge UID changed across replay: %s != %s", uid, replayUID)
	}
	assertReviewCutoverCount(t, reopened, "SELECT COUNT(*) FROM router_events WHERE id='task.completed' AND source='tasks'", 1)
	assertReviewCutoverCount(t, reopened, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", 0, source.ID)
}

func TestRouterReviewCutoverDisabledSettingDoesNotBypassPluginGate(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	s.UseRouterReviews()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.review.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	source := completeSoftwareTask(t, s)
	if children, err := s.TaskReviewChildren(ctx, "tasks", source.ID); err != nil || len(children) != 0 {
		t.Fatalf("disabled review setting was bypassed by completion: %+v %v", children, err)
	}
	assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", 0, source.ID)
	runtime := reviewCutoverRuntime(t, s)
	if _, err := s.DrainRouterTaskOutbox(ctx, runtime.Registry(), 200); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := s.db.QueryRowContext(ctx, `SELECT seq FROM router_events WHERE id='task.completed' AND json_extract(payload,'$.task.id')=?`, source.ID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	waitRouterDeliveryState(t, s, seq, "review", "stopped")
	stop()
	assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM task_subtasks WHERE parent_id=?", 0, source.ID)
}

func TestPublishLegacyReviewChangesDropsObsoleteObligations(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := routerReviewSource(t, s)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO task_review_outbox(source_task_id,source_done_at) VALUES(?,?)", source.ID, *source.DoneAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='todo',done_at=NULL WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	if _, count, err := s.PublishLegacyReviewChanges(ctx, nil, 200); err != nil || count != 1 {
		t.Fatalf("drop obsolete obligation count=%d err=%v", count, err)
	}
	assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", 0, source.ID)
	assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM router_events WHERE id='task.completed' AND source='tasks'", 0)
}

func TestPublishLegacyReviewChangesRequiresReviewController(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := routerReviewSource(t, s)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO task_review_outbox(source_task_id,source_done_at) VALUES(?,?)", source.ID, *source.DoneAt); err != nil {
		t.Fatal(err)
	}
	runtime, err := fism.NewRuntime(s, s.EventBus(), RouterTaskEventDefinitions(), []fism.Controller{
		controllers.RemoteController([]string{"task.completed"}),
	}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, count, err := s.PublishLegacyReviewChanges(ctx, runtime.Registry(), 200); err == nil || count != 0 {
		t.Fatalf("remote-only subscriber acknowledged review obligation: count=%d err=%v", count, err)
	}
	assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", 1, source.ID)
	assertReviewCutoverCount(t, s, "SELECT COUNT(*) FROM router_events WHERE id='task.completed' AND source='tasks'", 0)
}
