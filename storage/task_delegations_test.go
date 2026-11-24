package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDelegationReadinessGatesEveryNativeClaimAndPromptsForInspection(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	if err := s.InsertProject(ctx, fixtureProject("delegated-target")); err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	goal := "source goal"
	parent, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Parent", Reporter: "fixture", Goal: &goal}, actor)
	if err != nil {
		t.Fatal(err)
	}
	input := TaskDelegationInput{SourceProject: "tasks", TargetProject: "delegated-target", Title: "Implement", Reporter: "fixture", ParentID: &parent.ID}
	result, err := s.DelegateTask(ctx, input, actor)
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceTask.DelegationReady {
		t.Fatal("unfinished target reported ready")
	}
	if _, err := s.TransitionTask(ctx, input.SourceProject, result.SourceTask.ID, "in_progress", actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("source tracker claimed before target completion: %v", err)
	}
	eligible, err := assignmentEligible(ctx, s.db, result.SourceTask, assignmentCandidate{origin: *actor})
	if err != nil || eligible {
		t.Fatalf("unfinished delegated tracker eligible=%t err=%v", eligible, err)
	}

	if _, err := s.UpdateTaskWorkflow(ctx, input.TargetProject, result.TargetTask.ID, TaskWorkflowUpdate{
		Status: "done", Changeset: json.RawMessage("null"), Policy: TaskTransitionPolicy{HumanRequest: true},
	}, nil); err != nil {
		t.Fatal(err)
	}
	source, err := s.Task(ctx, input.SourceProject, result.SourceTask.ID)
	if err != nil || source == nil || !source.Task.DelegationReady || source.Task.OwnerOrigin != nil {
		t.Fatalf("target completion did not release and ready source tracker: %+v %v", source, err)
	}
	listed, err := s.Tasks(ctx, input.SourceProject, []string{"todo"}, true)
	listedReady := false
	for _, candidate := range listed {
		if candidate.ID == result.SourceTask.ID {
			listedReady = candidate.DelegationReady
		}
	}
	if err != nil || !listedReady {
		t.Fatalf("task list missed delegation readiness: %+v %v", listed, err)
	}
	eligible, err = assignmentEligible(ctx, s.db, source.Task, assignmentCandidate{origin: *actor})
	if err != nil || !eligible {
		t.Fatalf("completed target did not make source tracker assignable: eligible=%t err=%v", eligible, err)
	}
	prompt := AssignmentPrompt(&source.Task)
	if !strings.Contains(prompt, "Inspect that target task") || !strings.Contains(prompt, result.TargetTask.ID) || strings.Contains(prompt, "perform it") || strings.Contains(prompt, "Do not redo the delegated implementation") == false {
		t.Fatalf("delegated adoption prompt would redo work or omit inspection: %s", prompt)
	}
	claimed, err := s.TransitionTask(ctx, input.SourceProject, result.SourceTask.ID, "in_progress", actor)
	if err != nil || claimed.Status != "in_progress" {
		t.Fatalf("ready source tracker claim failed: %+v %v", claimed, err)
	}
	target, err := s.Task(ctx, input.TargetProject, result.TargetTask.ID)
	if err != nil || target == nil || !containsSystemComment(target.Comments, "selected an agent to inspect") {
		t.Fatalf("claim path omitted target inspection comment: %+v %v", target, err)
	}
}

func TestUnreadyDelegatedTrackerStillDeliversExplicitRequesterResults(t *testing.T) {
	actor := taskActor()
	requester := RegisteredTargetIdentity{Origin: *actor, Generation: "99999999-9999-4999-8999-999999999999"}
	task, err := json.Marshal(map[string]any{
		"id": "source-task", "kind": "delegated", "delegation_ready": false,
		"status": "done", "labels": []string{}, "owner_origin": nil,
		"requester": requester, "routing": map[string]string{"mode": "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	requesterAgent := notifierAgent{SessionID: actor.SessionID, InstanceID: actor.InstanceID, OS: actor.OS, Generation: &requester.Generation}
	if !routerNotifierRecipient("task_completed", task, json.RawMessage("null"), requesterAgent) {
		t.Fatal("source tracker readiness suppressed the explicit requester result")
	}
	if routerNotifierNewTask("task.created", task) {
		t.Fatal("unready source tracker was announced as newly actionable work")
	}
}

func TestDelegateTaskRejectsNilRequesterGeneration(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	if err := s.InsertProject(ctx, fixtureProject("delegated-target")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='tasks'"); err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	registered, err := s.RegisterTarget(ctx, "tasks", TargetRegistration{
		Origin: *actor, Generation: "99999999-9999-4999-8999-999999999999",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Parent", Reporter: "fixture"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	registered.Target.Generation = "00000000-0000-0000-0000-000000000000"
	input := TaskDelegationInput{
		SourceProject: "tasks", TargetProject: "delegated-target", Title: "Implement", Reporter: "fixture",
		ParentID: &parent.ID, Requester: &registered.Target,
	}
	if _, err := s.DelegateTask(ctx, input, actor); !errors.Is(err, ErrTaskDelegation) {
		t.Fatalf("nil requester generation error=%v, want ErrTaskDelegation", err)
	}
}
