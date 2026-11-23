package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestDelegationUpdateClientAndNativeToolPreserveBothContractsAndAuthority(t *testing.T) {
	s, api, parent, owner := requesterClientFixture(t)
	ctx := context.Background()
	host := httptest.NewServer(api)
	defer host.Close()
	client, _ := New(host.URL, "requester-fixture", host.Client())
	delegated, err := client.DelegateTask(ctx, "source", TaskDelegateInput{TargetProject: "target", Title: "Original", ParentID: &parent.ID, Labels: []string{"keep"}}, &owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := delegationTestActor()
	if _, err := client.TaskUpdate(ctx, "target", delegated.TargetTask.ID, storage.TaskWorkflowUpdate{Status: "in_progress"}, &executor, nil); err != nil {
		t.Fatal(err)
	}
	editor := delegationTestActor()
	title, description, specification := "Reviewed", "", "literal\n$(context)"
	updated, err := client.UpdateTaskDelegation(ctx, "source", delegated.SourceTask.ID, storage.TaskDelegationUpdate{Title: &title, Description: &description, Specification: &specification, Labels: []string{}}, &editor)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SourceTask.OwnerOrigin == nil || *updated.SourceTask.OwnerOrigin != owner || updated.TargetTask.OwnerOrigin == nil || *updated.TargetTask.OwnerOrigin != executor || updated.SourceTask.Title != "Reviewed (DELEGATED)" || updated.TargetTask.Title != "Reviewed" || len(updated.SourceTask.Labels) != 0 || len(updated.TargetTask.Labels) != 0 {
		t.Fatal("text edit changed owners or failed to mirror metadata")
	}
	sourceBefore, _ := s.Task(ctx, "source", delegated.SourceTask.ID)
	targetBefore, _ := s.Task(ctx, "target", delegated.TargetTask.ID)
	if _, err := client.UpdateTaskDelegation(ctx, "source", delegated.SourceTask.ID, storage.TaskDelegationUpdate{Labels: []string{"os:windows"}}, &owner); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("source owner bypassed target OS owner fence: %v", err)
	}
	sourceAfter, _ := s.Task(ctx, "source", delegated.SourceTask.ID)
	targetAfter, _ := s.Task(ctx, "target", delegated.TargetTask.ID)
	if sourceAfter.Task.OwnerOrigin == nil || *sourceAfter.Task.OwnerOrigin != owner || len(sourceAfter.Task.Labels) != 0 || len(targetAfter.Task.Labels) != 0 || sourceAfter.CommentCount != sourceBefore.CommentCount || targetAfter.CommentCount != targetBefore.CommentCount {
		t.Fatal("rejected paired OS edit left partial changes")
	}
	work, err := s.InsertTask(ctx, storage.TaskInsert{Project: "source", Title: "Editor context", Reporter: editor.SessionID}, &editor)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := s.CreateConversation(ctx, "source", work.ID, &editor)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"id": delegated.SourceTask.ID, "title": "Native edited", "labels": []string{}})
	result, err := client.CallTaskTool(ctx, "source", conversation.ID, editor, "task_update_delegation", args)
	if err != nil || result.IsError {
		t.Fatalf("native paired update failed: %+v %v", result, err)
	}
	if _, err := client.TaskComment(ctx, "source", delegated.SourceTask.ID, "Please keep the examples.", &editor); err != nil {
		t.Fatal(err)
	}
	targetDetail, err := s.Task(ctx, "target", delegated.TargetTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	clarified := false
	for _, comment := range targetDetail.Comments {
		clarified = clarified || comment.AuthorType == "system" && strings.Contains(comment.Content, "Please keep the examples.")
	}
	if !clarified {
		t.Fatal("source clarification did not reach the target task")
	}
	sourceDetail, _ := s.Task(ctx, "source", delegated.SourceTask.ID)
	if _, err := client.TaskComment(ctx, "target", delegated.TargetTask.ID, "Examples are updated.", &executor); err != nil {
		t.Fatal(err)
	}
	sourceFinal, _ := s.Task(ctx, "source", delegated.SourceTask.ID)
	if sourceFinal.CommentCount != sourceDetail.CommentCount {
		t.Fatal("target response was incorrectly duplicated as a source task comment")
	}
}
