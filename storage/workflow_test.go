package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
)

func TestChangesetValidation(t *testing.T) {
	for _, raw := range []string{`null`, `["src/main.go"]`, `["a","a"]`, `["file with spaces","line\nbreak"]`} {
		if _, err := ValidateChangeset(json.RawMessage(raw)); err != nil {
			t.Fatalf("valid changeset %s: %v", raw, err)
		}
	}
	for _, raw := range []string{`[]`, `{}`, `"file"`, `[null]`, `[4]`, `[""]`, `["  "]`, `["bad\u0000path"]`} {
		if _, err := ValidateChangeset(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid changeset accepted: %s", raw)
		}
	}
	raw, _ := json.Marshal([]string{strings.Repeat("x", 4097)})
	if _, err := ValidateChangeset(raw); err == nil {
		t.Fatal("oversized path accepted")
	}
}

func TestAnyAgentCanCompleteOrdinaryTaskWithoutStealingOwner(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	owner := taskActor()
	task := makeTask(t, s, owner)
	closer := *owner
	closer.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	closer.Provider = "claude"
	done, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`null`)}, &closer)
	if err != nil || done.Status != "done" || done.DoneAt == nil || done.OwnerOrigin == nil || *done.OwnerOrigin != *owner {
		t.Fatalf("completion %+v, %v", done, err)
	}
	events, err := s.TaskEvents(ctx, EventQuery{})
	if err != nil || events[0].Kind != "task_completed" || events[0].Origin == nil || *events[0].Origin != closer {
		t.Fatalf("actual closer not recorded: %+v, %v", events, err)
	}
	if reviews, err := s.TaskReviewChildren(ctx, "tasks", task.ID); err != nil || len(reviews) != 0 {
		t.Fatalf("general task created reviews: %+v, %v", reviews, err)
	}
}

func TestGuidedMutationConfirmationAndReviewIdempotency(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done"}, actor); !errors.Is(err, ErrTaskGuidedQuestion) {
		t.Fatalf("missing guided question: %v", err)
	}
	before, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || before.Task.Status != "todo" {
		t.Fatal("question moved task")
	}
	if _, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`["main.go"]`)}, actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("files accepted before mutation confirmation: %v", err)
	}
	confirmed, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{TaskType: ptr("software")}, actor)
	if err != nil || !confirmed.NeedsChangeset {
		t.Fatalf("mutation confirmation %+v, %v", confirmed, err)
	}
	if _, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`null`)}, actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("confirmed mutation discarded: %v", err)
	}
	completed, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`["main.go","main.go","docs/usage.md"]`)}, actor)
	if err != nil || completed.Task == nil || completed.Task.Status != "done" || len(completed.Task.Changeset) != 2 {
		t.Fatalf("completed %+v, %v", completed, err)
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", task.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("reviews %+v, %v", reviews, err)
	}
	for _, review := range reviews {
		if review.OwnerOrigin != nil || review.Status != "todo" || !hasLabel(review.Labels, "review-exclude:"+actor.SessionID) || !hasLabel(review.Labels, "review-provider:claude") {
			t.Fatalf("review independence %+v", review)
		}
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done"}, actor); err != nil {
		t.Fatal(err)
	}
	reviews, err = s.TaskReviewChildren(ctx, "tasks", task.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatal("repeated completion duplicated reviews")
	}
}

func TestParkingKeepsCompletionOwnerAndRequiresHumanDecision(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	owner := taskActor()
	task := makeTask(t, s, owner)
	parked, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "user_review", ReviewAction: ptr("Choose the release date"), Changeset: json.RawMessage(`["main.go"]`)}, owner)
	if err != nil || parked.OwnerOrigin != nil || parked.OwnerAgentID != nil || parked.StartedAt != nil || parked.CompletionOwner == nil || *parked.CompletionOwner != *owner {
		t.Fatalf("parking %+v, %v", parked, err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "todo", Changeset: json.RawMessage(`null`)}, owner); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("agent overrode user review: %v", err)
	}
	closer := *owner
	closer.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	closer.Provider = "claude"
	done, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`null`)}, &closer)
	if err != nil || done.OwnerOrigin == nil || *done.OwnerOrigin != *owner || done.ReviewAction != nil || len(done.Changeset) != 1 {
		t.Fatalf("parked completion %+v, %v", done, err)
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", task.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("parked reviews %+v, %v", reviews, err)
	}
	for _, review := range reviews {
		if !hasLabel(review.Labels, "review-provider:claude") || !hasLabel(review.Labels, "review-exclude:"+owner.SessionID) || hasLabel(review.Labels, "review-exclude:"+closer.SessionID) {
			t.Fatalf("closer changed reviewer independence: %+v", review)
		}
	}
}

func TestPendingConfirmationRejectsChangedTaskAndOtherInstance(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	owner := taskActor()
	task := makeTask(t, s, owner)
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done"}, owner); !errors.Is(err, ErrTaskGuidedQuestion) {
		t.Fatal(err)
	}
	other := *owner
	other.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`null`)}, &other); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("other execution confirmed: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET title='Changed' WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`null`)}, owner); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("stale confirmation accepted: %v", err)
	}
}

func TestReviewQueueFailureDoesNotUndoCompletionAndCanRecover(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_review_queue BEFORE INSERT ON tasks WHEN NEW.reporter='amux-autoreview' BEGIN SELECT RAISE(ABORT,'review worker fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	done, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, actor)
	if err != nil || done.Status != "done" {
		t.Fatalf("review failure blocked closure: %+v, %v", done, err)
	}
	var pending int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_review_outbox WHERE source_task_id=?", task.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("review obligation lost: %d, %v", pending, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_review_queue"); err != nil {
		t.Fatal(err)
	}
	if failed, err := s.RetryTaskReviews(ctx); err != nil || failed {
		t.Fatalf("review retry %v, %v", failed, err)
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", task.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("recovery reviews %+v, %v", reviews, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_review_outbox").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("completed obligation retained: %d, %v", pending, err)
	}
}

// Rust routes.rs::later_null_confirmation_keeps_earlier_files_and_reviews.
func TestLaterNullDeclarationKeepsFilesAndReviews(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	for _, update := range []TaskWorkflowUpdate{{Status: "in_progress"}, {Status: "todo", Changeset: json.RawMessage(`["x"]`)}, {Status: "in_progress"}, {Status: "done", Changeset: json.RawMessage(`null`)}} {
		result, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, update, actor)
		if err != nil {
			t.Fatal(err)
		}
		task = result
	}
	if task.TaskType != "software" || len(task.Changeset) != 1 || task.Changeset[0] != "x" {
		t.Fatalf("prior mutation lost: %+v", task)
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", task.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("reviews %+v, %v", reviews, err)
	}
}

// Rust task_reviews.rs::documentation_workflow_descendants_do_not_start_another_review_cycle.
func TestReviewDescendantsDoNotGenerateReviewChains(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	source := makeTask(t, s, actor)
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, actor); err != nil {
		t.Fatal(err)
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", source.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, review := range reviews {
		if _, err := s.UpdateTaskWorkflow(ctx, "tasks", review.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["review.md"]`)}, actor); err != nil {
			t.Fatal(err)
		}
		grandchildren, err := s.TaskReviewChildren(ctx, "tasks", review.ID)
		if err != nil || len(grandchildren) != 0 {
			t.Fatal("review recursively generated another review")
		}
		if hasLabel(review.Labels, "documentation-review") {
			child, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Documentation correction", Reporter: "fixture", ParentID: &review.ID}, actor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateTaskWorkflow(ctx, "tasks", child.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["docs/usage.md"]`)}, actor); err != nil {
				t.Fatal(err)
			}
			grandchildren, err = s.TaskReviewChildren(ctx, "tasks", child.ID)
			if err != nil || len(grandchildren) != 0 {
				t.Fatal("documentation descendant generated another review cycle")
			}
		}
	}
}

func TestReviewWorkerRestartsFromDurableObligation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	source := makeTask(t, s, taskActor())
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_review_queue BEFORE INSERT ON tasks WHEN NEW.reporter='amux-autoreview' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, taskActor()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_review_queue"); err != nil {
		t.Fatal(err)
	}
	changed := s.TaskEventChanges()
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.RunTaskReviewWorker(workerCtx) }()
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not recover durable review work")
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", source.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("restart reviews %+v, %v", reviews, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("review worker did not stop")
	}
}

// Rust task_reviews.rs::legacy_review_chain_migrates_without_duplicate_workflows.
func TestLegacyReviewChainMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-reviews.db")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	source := makeTask(t, s, taskActor())
	source, err = s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, taskActor())
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", source.ID)
	if err != nil {
		t.Fatal(err)
	}
	var code, docs Task
	for _, review := range reviews {
		if hasLabel(review.Labels, "documentation-review") {
			docs = review
		} else {
			code = review
		}
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE task_reviews RENAME TO new_reviews;CREATE TABLE task_reviews(source_task_id TEXT NOT NULL,source_done_at INTEGER NOT NULL,review_task_id TEXT NOT NULL,PRIMARY KEY(source_task_id,source_done_at));INSERT INTO task_reviews SELECT source_task_id,source_done_at,review_task_id FROM new_reviews WHERE stage='code';`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO task_reviews VALUES(?,?,?)", code.ID, *source.DoneAt, docs.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE new_reviews"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, err := s.EnqueueTaskReviews(ctx, "tasks", source.ID)
	if err != nil || len(again) != 2 {
		t.Fatalf("migrated reviews %+v, %v", again, err)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM task_reviews WHERE source_task_id=?", source.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate review workflow: %d, %v", count, err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal("idempotent migration:", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET labels='[]',status='done',done_at=1 WHERE id=?", docs.ID); err != nil {
		t.Fatal(err)
	}
	followup, err := s.EnqueueTaskReviews(ctx, "tasks", docs.ID)
	if err != nil || len(followup) != 0 {
		t.Fatalf("legacy workflow recursively reviewed: %+v, %v", followup, err)
	}
}

func TestReviewRetryHonorsDisabledPlugin(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	source := makeTask(t, s, taskActor())
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_review_queue BEFORE INSERT ON tasks WHEN NEW.reporter='amux-autoreview' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`["main.go"]`)}, taskActor()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_review_queue;INSERT INTO settings(key,value) VALUES('plugin.review.enabled','false')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryTaskReviews(ctx); err != nil {
		t.Fatal(err)
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", source.ID)
	if err != nil || len(reviews) != 0 {
		t.Fatal("disabled automatic review plugin created work")
	}
	// Manual review requests are still supported while automatic reviews are off.
	reviews, err = s.EnqueueTaskReviews(ctx, "tasks", source.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("manual review request %+v, %v", reviews, err)
	}
}

func TestCompletionAllowedFromEveryOrdinaryLane(t *testing.T) {
	for _, lane := range []string{"todo", "in_progress", "user_review", "cancelled", "done"} {
		for _, human := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/human=%v", lane, human), func(t *testing.T) {
				s := taskStore(t)
				ctx := context.Background()
				source := makeTask(t, s, taskActor())
				if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status=?,done_at=CASE WHEN ?='done' THEN 1 ELSE NULL END WHERE id=?", lane, lane, source.ID); err != nil {
					t.Fatal(err)
				}
				var closer *agent.Origin
				if !human {
					actor := *taskActor()
					actor.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
					closer = &actor
				}
				result, err := s.UpdateTaskWorkflow(ctx, "tasks", source.ID, TaskWorkflowUpdate{Status: "done", Changeset: json.RawMessage(`null`), Policy: TaskTransitionPolicy{HumanRequest: human}}, closer)
				if err != nil || result.Status != "done" {
					t.Fatalf("ordinary closure blocked: %+v, %v", result, err)
				}
			})
		}
	}
}
