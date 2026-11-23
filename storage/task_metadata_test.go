package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Mirrors routes.rs::update_task_handler nullable decoding and specification precedence.
func TestTaskMetadataWireValidationAndSpecificationPrecedence(t *testing.T) {
	for _, raw := range []string{`{"description":null}`, `{"labels":null}`, `{"labels":[]}`, `{"goal":"ship"}`} {
		var update TaskWorkflowUpdate
		if err := json.Unmarshal([]byte(raw), &update); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if err := ValidateTaskMetadata(update.TaskMetadataUpdate); err != nil {
			t.Fatalf("validate %s: %v", raw, err)
		}
	}
	for _, raw := range []string{`{"description":4}`, `{"goal":{} }`, `{"labels":[null]}`, `{"labels":"os:linux"}`} {
		var update TaskWorkflowUpdate
		if err := json.Unmarshal([]byte(raw), &update); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if err := ValidateTaskMetadata(update.TaskMetadataUpdate); !errors.Is(err, ErrTaskMetadataInput) {
			t.Fatalf("invalid metadata %s returned %v", raw, err)
		}
	}

	var update TaskWorkflowUpdate
	if err := json.Unmarshal([]byte(`{"specification":null,"details":"legacy"}`), &update); err != nil {
		t.Fatal(err)
	}
	if got := update.TaskMetadataUpdate.DetailsValue(); string(got) != `null` {
		t.Fatalf("present null specification must take precedence over details, got %s", got)
	}
	var ignoredDetails TaskWorkflowUpdate
	if err := json.Unmarshal([]byte(`{"specification":null,"details":123}`), &ignoredDetails); err != nil || ValidateTaskMetadata(ignoredDetails.TaskMetadataUpdate) != nil {
		t.Fatalf("details must be ignored when specification is present: %v", err)
	}
	if title, err := json.Marshal(TaskWorkflowUpdate{TaskMetadataUpdate: TaskMetadataUpdate{}}); err != nil || strings.Contains(string(title), `"title":null`) {
		t.Fatalf("omitted title changed wire shape: %s, %v", title, err)
	}
	var nullableTitle TaskWorkflowUpdate
	if err := json.Unmarshal([]byte(`{"title":null}`), &nullableTitle); err != nil || nullableTitle.Title != nil {
		t.Fatalf("Rust nullable title semantics: %+v, %v", nullableTitle, err)
	}
}

func TestMetadataOnlyUpdateAndNullableFields(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	task := makeTask(t, s, taskActor())
	update := TaskWorkflowUpdate{TaskMetadataUpdate: TaskMetadataUpdate{
		Description: json.RawMessage(`null`), Details: json.RawMessage(`"spec"`),
		Goal: json.RawMessage(`"Ship"`), Hostname: json.RawMessage(`null`),
		Labels: json.RawMessage(`[]`), Rank: int64ptr(8),
	}}
	got, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, update, nil)
	if err != nil || got.Status != "todo" || got.Description != nil || got.Details == nil || *got.Details != "spec" || got.Goal == nil || *got.Goal != "Ship" || got.Hostname != nil || len(got.Labels) != 0 || got.Rank != 8 {
		t.Fatalf("metadata-only update: %+v, %v", got, err)
	}
}

func TestTaskMetadataGoalAndClaimUseExistingGoalContext(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET goal='Original' WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{
		Status:             "in_progress",
		TaskMetadataUpdate: TaskMetadataUpdate{Goal: json.RawMessage(`"New"`)},
		Policy:             TaskTransitionPolicy{CheckGoal: true, SelectedGoal: "Original"},
	}, actor)
	if err != nil || updated.Status != "in_progress" || updated.Goal == nil || *updated.Goal != "New" {
		t.Fatalf("goal change and claim should validate against original goal: %+v, %v", updated, err)
	}
}

func TestCombinedCompletionMetadataAppearsInDurableSnapshot(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	got, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage(`["main.go"]`),
		TaskMetadataUpdate: TaskMetadataUpdate{Title: stringptr("Finished"), Rank: int64ptr(13), Details: json.RawMessage(`"new details"`), Labels: json.RawMessage(`["release"]`)},
	}, actor)
	if err != nil || got.Title != "Finished" || got.Rank != 13 || got.Details == nil || *got.Details != "new details" {
		t.Fatalf("combined update: %+v, %v", got, err)
	}
	events, err := s.TaskEvents(ctx, EventQuery{TaskID: &task.ID})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "task_completed" {
			var snapshot Task
			if err := json.Unmarshal(event.Task, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Title != "Finished" || snapshot.Rank != 13 || snapshot.Details == nil || *snapshot.Details != "new details" {
				t.Fatalf("completion snapshot lacks final metadata: %+v", snapshot)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing durable completion snapshot")
	}
	reviews, err := s.TaskReviewChildren(ctx, "tasks", task.ID)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("completion reviews: task=%+v reviews=%+v, %v", got, reviews, err)
	}
	for _, review := range reviews {
		if !strings.HasSuffix(review.Title, ": Finished") {
			t.Fatalf("review generation did not use final metadata: %+v", review)
		}
	}
}

func TestLegacyFlatPendingConfirmationSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/amux.db"
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	task := makeTask(t, s, actor)
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "done"}, actor); !errors.Is(err, ErrTaskGuidedQuestion) {
		t.Fatalf("guided request: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE task_confirmations SET request_json=? WHERE task_id=?", `{"status":"done"}`, task.ID); err != nil {
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
	result, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`null`)}, actor)
	if err != nil || result.Task == nil || result.Task.Status != "done" {
		t.Fatalf("reopened legacy confirmation: %+v, %v", result, err)
	}
}

func TestReviewActionOnlyHumanUpdateAndEmptyRequest(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "user_review", ReviewAction: stringptr("Choose one"), Changeset: json.RawMessage(`null`)}, actor); err != nil {
		t.Fatal(err)
	}
	before, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{ReviewAction: stringptr("Choose the launch date"), Policy: TaskTransitionPolicy{HumanRequest: true}}, nil)
	if err != nil || updated.ReviewAction == nil || *updated.ReviewAction != "Choose the launch date" {
		t.Fatalf("review action update: %+v, %v", updated, err)
	}
	noop, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{}, nil)
	if err != nil || noop.Status != before.Task.Status {
		t.Fatalf("empty update: %+v, %v", noop, err)
	}
}

func TestGuidedConfirmationRollsBackAndAppliesMetadataOnce(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET goal='Original' WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	request := TaskWorkflowUpdate{Status: "done", TaskMetadataUpdate: TaskMetadataUpdate{
		Title: stringptr("Guided"), Details: json.RawMessage(`"saved"`), Goal: json.RawMessage(`"New goal"`),
	}, Policy: TaskTransitionPolicy{CheckGoal: true, SelectedGoal: "Original"}}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, request, actor); !errors.Is(err, ErrTaskGuidedQuestion) {
		t.Fatalf("expected guided confirmation: %v", err)
	}
	before, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || before.Task.Title != task.Title || before.Task.Status != "todo" || before.Task.Goal == nil || *before.Task.Goal != "Original" {
		t.Fatalf("preview leaked metadata or status: %+v, %v", before, err)
	}
	if _, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`null`)}, actor); err != nil {
		t.Fatal(err)
	}
	after, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || after.Task.Title != "Guided" || after.Task.Details == nil || *after.Task.Details != "saved" || after.Task.Goal == nil || *after.Task.Goal != "New goal" || after.Task.Status != "done" {
		t.Fatalf("confirmation did not replay metadata: %+v, %v", after, err)
	}
	if _, err := s.ConfirmTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowConfirmation{Changeset: json.RawMessage(`null`)}, actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("metadata request applied more than once: %v", err)
	}
}

func TestMetadataReviewOSAndGoalGuards(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, actor)
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET labels=? WHERE id=?", ` ["task-review","review-of:source"] `, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{TaskMetadataUpdate: TaskMetadataUpdate{Labels: json.RawMessage(`[]`)}}, nil); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("review metadata changed: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET labels='[]',goal='Ship' WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{TaskMetadataUpdate: TaskMetadataUpdate{Goal: json.RawMessage(`"Other"`)}, Policy: TaskTransitionPolicy{CheckGoal: true}}, actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("goal changed without selected goal: %v", err)
	}
	goalPolicy := TaskTransitionPolicy{CheckGoal: true, SelectedGoal: "Ship"}
	otherOwner := *actor
	otherOwner.SessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{TaskMetadataUpdate: TaskMetadataUpdate{Goal: json.RawMessage(`"Other"`)}, Policy: goalPolicy}, &otherOwner); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("goal changed by a different owner session: %v", err)
	}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{TaskMetadataUpdate: TaskMetadataUpdate{Goal: json.RawMessage(`"Other"`), Labels: json.RawMessage(` ["os:windows"] `)}, Policy: goalPolicy}, actor); err != nil {
		t.Fatalf("authorized metadata update: %v", err)
	}
	current, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || !hasLabel(current.Task.Labels, "os:windows") || current.Task.Status != "todo" || current.Task.OwnerOrigin != nil || current.Task.OwnerAgentID != nil || current.Task.StartedAt != nil {
		t.Fatalf("OS metadata not applied: %+v, %v", current, err)
	}
	_, err = s.UpdateTaskWorkflow(ctx, "tasks", task.ID, TaskWorkflowUpdate{Status: "in_progress", TaskMetadataUpdate: TaskMetadataUpdate{Labels: json.RawMessage(` ["os:linux"] `), Goal: json.RawMessage(`null`)}, Policy: goalPolicy}, actor)
	if !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("OS change combined with status/goal clear was accepted: %v", err)
	}
	finding := makeTask(t, s, actor)
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET labels=? WHERE id=?", `["autoreview-finding"]`, finding.ID); err != nil {
		t.Fatal(err)
	}
	clearHold := TaskWorkflowUpdate{TaskMetadataUpdate: TaskMetadataUpdate{Labels: json.RawMessage(`[]`)}}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", finding.ID, clearHold, actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("agent cleared autoreview finding hold: %v", err)
	}
	clearHold.Policy = TaskTransitionPolicy{HumanRequest: true}
	if _, err := s.UpdateTaskWorkflow(ctx, "tasks", finding.ID, clearHold, nil); err != nil {
		t.Fatalf("user could not clear autoreview finding hold: %v", err)
	}
}

func stringptr(s string) *string { return &s }
func int64ptr(v int64) *int64    { return &v }
