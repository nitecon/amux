package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nitecon/amux/fism"
)

func routerReviewEvent(task Task, project string) fism.Event {
	payload, _ := json.Marshal(struct {
		Project string `json:"project"`
		Task    Task   `json:"task"`
	}{Project: project, Task: task})
	return fism.Event{ID: "task.completed", Version: 1, Payload: payload}
}

func routerReviewCounts(t *testing.T, s *Store, source string) (int, int) {
	t.Helper()
	var tasks, reviews int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM task_subtasks WHERE parent_id=?", source).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM task_reviews WHERE source_task_id=?", source).Scan(&reviews); err != nil {
		t.Fatal(err)
	}
	return tasks, reviews
}

func routerReviewSource(t *testing.T, s *Store) *Task {
	t.Helper()
	ctx := context.Background()
	source := makeTask(t, s, taskActor())
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET task_type='software',status='done',done_at=12345 WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Task(ctx, "tasks", source.ID)
	if err != nil || detail == nil {
		t.Fatalf("load completed source: %+v %v", detail, err)
	}
	return &detail.Task
}

func TestReconcileRouterReviewsReplayAndCompletionIdentity(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := routerReviewSource(t, s)
	event := routerReviewEvent(*source, "tasks")
	for i := 0; i < 2; i++ {
		processed, err := s.ReconcileRouterReviews(ctx, event)
		if err != nil || !processed {
			t.Fatalf("reconcile %d: processed=%v err=%v", i, processed, err)
		}
	}
	if tasks, reviews := routerReviewCounts(t, s, source.ID); tasks != 2 || reviews != 2 {
		t.Fatalf("replay duplicated or omitted reviews: tasks=%d relations=%d", tasks, reviews)
	}
	var codeTitle string
	if err := s.db.QueryRow("SELECT t.title FROM tasks t JOIN task_subtasks s ON s.child_id=t.id WHERE s.parent_id=? ORDER BY t.title LIMIT 1", source.ID).Scan(&codeTitle); err != nil {
		t.Fatal(err)
	}
	if codeTitle != "Documentation review: Work" && codeTitle != "Review: Work" {
		t.Fatalf("unexpected generated review title %q", codeTitle)
	}

	// An absent timestamp is the legacy id-only payload and binds to current state.
	legacy, _ := json.Marshal(struct {
		Project string `json:"project"`
		Task    struct {
			ID string `json:"id"`
		} `json:"task"`
	}{Project: "tasks", Task: struct {
		ID string `json:"id"`
	}{ID: source.ID}})
	if processed, err := s.ReconcileRouterReviews(ctx, fism.Event{ID: "task.completed", Version: 1, Payload: legacy}); err != nil || !processed {
		t.Fatalf("legacy id-only payload: processed=%v err=%v", processed, err)
	}

	for _, raw := range []string{`{"project":"tasks","task":{"id":"` + source.ID + `","done_at":null}}`, `{"project":"tasks","task":{"id":"` + source.ID + `","done_at":"bad"}}`} {
		processed, err := s.ReconcileRouterReviews(ctx, fism.Event{ID: "task.completed", Version: 1, Payload: json.RawMessage(raw)})
		if err != nil || processed {
			t.Fatalf("invalid completion snapshot %s: processed=%v err=%v", raw, processed, err)
		}
	}
	old := *source.DoneAt - 1
	stale, _ := json.Marshal(struct {
		Project string `json:"project"`
		Task    Task   `json:"task"`
	}{Project: "tasks", Task: *source})
	var snapshot map[string]any
	if err := json.Unmarshal(stale, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot["task"].(map[string]any)["done_at"] = old
	stale, _ = json.Marshal(snapshot)
	if processed, err := s.ReconcileRouterReviews(ctx, fism.Event{ID: "task.completed", Version: 1, Payload: stale}); err != nil || processed {
		t.Fatalf("stale completion snapshot: processed=%v err=%v", processed, err)
	}
}

func TestReconcileRouterReviewsStopsForObsoleteTasksAndRejectsEnvelope(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := routerReviewSource(t, s)
	for _, payload := range []string{
		`{}`, `{"project":"tasks","task":{}}`,
		`{"project":"tasks","task":{"id":"missing"}}`,
	} {
		processed, err := s.ReconcileRouterReviews(ctx, fism.Event{ID: "task.completed", Version: 1, Payload: json.RawMessage(payload)})
		if err != nil || processed {
			t.Fatalf("invalid/missing task payload %s: processed=%v err=%v", payload, processed, err)
		}
	}
	for _, event := range []fism.Event{{ID: "task.updated", Version: 1}, {ID: "task.completed", Version: 2}} {
		if _, err := s.ReconcileRouterReviews(ctx, event); err == nil {
			t.Fatalf("accepted unsupported envelope %+v", event)
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='todo' WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	if processed, err := s.ReconcileRouterReviews(ctx, routerReviewEvent(*source, "tasks")); err != nil || processed {
		t.Fatalf("reopened task: processed=%v err=%v", processed, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='done',done_at=12345,task_type='general' WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	if processed, err := s.ReconcileRouterReviews(ctx, routerReviewEvent(*source, "tasks")); err != nil || processed {
		t.Fatalf("nonsoftware task: processed=%v err=%v", processed, err)
	}
	if tasks, reviews := routerReviewCounts(t, s, source.ID); tasks != 0 || reviews != 0 {
		t.Fatalf("stopped event generated reviews: tasks=%d relations=%d", tasks, reviews)
	}
}

func TestReconcileRouterReviewsGenerationErrorRollsBack(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := routerReviewSource(t, s)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_router_review BEFORE INSERT ON tasks WHEN NEW.reporter='amux-autoreview' AND NEW.title LIKE 'Documentation review:%' BEGIN SELECT RAISE(ABORT,'fixture review failure'); END`); err != nil {
		t.Fatal(err)
	}
	if processed, err := s.ReconcileRouterReviews(ctx, routerReviewEvent(*source, "tasks")); err == nil || processed {
		t.Fatalf("generation failure: processed=%v err=%v", processed, err)
	}
	if tasks, reviews := routerReviewCounts(t, s, source.ID); tasks != 0 || reviews != 0 {
		t.Fatalf("failed generation was not rolled back: tasks=%d relations=%d", tasks, reviews)
	}
}

func TestReconcileRouterReviewsChecksEnabledSettingInTransaction(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := routerReviewSource(t, s)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.review.enabled','false')"); err != nil {
		t.Fatal(err)
	}
	if processed, err := s.ReconcileRouterReviews(ctx, routerReviewEvent(*source, "tasks")); err != nil || processed {
		t.Fatalf("disabled plugin: processed=%v err=%v", processed, err)
	}
	if tasks, reviews := routerReviewCounts(t, s, source.ID); tasks != 0 || reviews != 0 {
		t.Fatalf("disabled plugin generated reviews: tasks=%d relations=%d", tasks, reviews)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE settings SET value='true' WHERE key='plugin.review.enabled'"); err != nil {
		t.Fatal(err)
	}
	if processed, err := s.ReconcileRouterReviews(ctx, routerReviewEvent(*source, "tasks")); err != nil || !processed {
		t.Fatalf("enabled plugin: processed=%v err=%v", processed, err)
	}
}

func TestReconcileRouterReviewsUsesSourceCompletionWorkflow(t *testing.T) {
	for _, fixture := range []struct {
		name, initial string
		human         bool
	}{
		{name: "human todo", initial: "todo", human: true},
		{name: "human in progress", initial: "in_progress", human: true},
		{name: "human user review", initial: "user_review", human: true},
		{name: "agent in progress", initial: "in_progress"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			s := taskStore(t)
			ctx := context.Background()
			actor := taskActor()
			source := makeTask(t, s, actor)
			if _, err := s.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('plugin.review.enabled','false')"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET task_type='software',status=? WHERE id=?", fixture.initial, source.ID); err != nil {
				t.Fatal(err)
			}
			policy := TaskTransitionPolicy{HumanRequest: fixture.human}
			completionActor := actor
			if fixture.human {
				completionActor = nil
			}
			done, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`), Policy: policy}, completionActor)
			if err != nil || done.Status != "done" || done.DoneAt == nil {
				t.Fatalf("source completion: %+v %v", done, err)
			}
			if done.TaskType != "software" {
				t.Fatalf("source completion task type %q", done.TaskType)
			}
			if _, err := s.db.ExecContext(ctx, "UPDATE settings SET value='true' WHERE key='plugin.review.enabled'"); err != nil {
				t.Fatal(err)
			}
			processed, err := s.ReconcileRouterReviews(ctx, routerReviewEvent(*done, "tasks"))
			if err != nil || !processed {
				t.Fatalf("router reconciliation: processed=%v err=%v", processed, err)
			}
			if tasks, reviews := routerReviewCounts(t, s, source.ID); tasks != 2 || reviews != 2 {
				t.Fatalf("source completion generated tasks=%d relations=%d", tasks, reviews)
			}
			var providerLabel int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks t JOIN task_subtasks s ON s.child_id=t.id WHERE s.parent_id=? AND t.labels LIKE '%review-provider:claude%'", source.ID).Scan(&providerLabel); err != nil || providerLabel != 2 {
				t.Fatalf("source-derived provider metadata count=%d err=%v", providerLabel, err)
			}
		})
	}
}
