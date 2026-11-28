package gatewayclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestWorkerAssignmentGoalHTTPGuardsAdoptionAndRetry(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "goal-adoption.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	goal := "release"
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Scoped assignment", Reporter: "user", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	if _, err := s.ApplyTaskTransition(ctx, "demo", task.ID, "in_progress", &owner, storage.TaskTransitionPolicy{CheckGoal: true, SelectedGoal: goal}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, err := New(host.URL, "key", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "other"
	for _, selected := range []*string{nil, &wrong} {
		if adopted, err := client.AdoptWorkerAssignmentWithGoal(ctx, "demo", owner, selected); err == nil || adopted != nil {
			t.Fatalf("mismatched goal adopted work: %+v %v", adopted, err)
		} else {
			var response *HTTPError
			if !errors.As(err, &response) || response.Status != http.StatusConflict {
				t.Fatalf("mismatched goal error = %v", err)
			}
		}
	}
	padded := " release "
	first, err := client.AdoptWorkerAssignmentWithGoal(ctx, "demo", owner, &padded)
	if err != nil || first == nil || first.Task.ID != task.ID {
		t.Fatalf("selected adoption: %+v %v", first, err)
	}
	second, err := client.AdoptWorkerAssignmentWithGoal(ctx, "demo", owner, &goal)
	if err != nil || second == nil || second.RunID != first.RunID || second.Conversation.ID != first.Conversation.ID {
		t.Fatalf("selected retry lost binding: %+v %v", second, err)
	}
	if _, err := client.AdoptWorkerAssignment(ctx, "demo", owner); err == nil {
		t.Fatal("existing adopted run bypassed selected goal")
	}
	commands, err := s.ConversationCommands(ctx, "demo", first.Conversation.ID, 0, 100)
	if err != nil || len(commands) != 1 || commands[0].Command.Kind != "prompt" {
		t.Fatalf("goal retry duplicated initial prompt: %+v %v", commands, err)
	}
	invalid := "invalid\ngoal"
	if _, err := client.AdoptWorkerAssignmentWithGoal(ctx, "demo", owner, &invalid); err == nil {
		t.Fatal("invalid selected goal accepted")
	}
	manual, err := s.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Manually selected goal work", Reporter: "user", Goal: &goal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manualOwner := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	for _, selected := range []*string{nil, &wrong} {
		_, err := client.CreateConversationWithGoal(ctx, "demo", manual.ID, manualOwner, selected)
		var response *HTTPError
		if !errors.As(err, &response) || response.Status != http.StatusConflict {
			t.Fatalf("manual initial claim bypassed goal: %v", err)
		}
	}
	unchanged, err := s.Task(ctx, "demo", manual.ID)
	if err != nil || unchanged.Task.OwnerOrigin != nil || unchanged.Task.Status != "todo" {
		t.Fatalf("rejected conversation changed task: %+v %v", unchanged, err)
	}
	conversations, err := s.TaskConversations(ctx, "demo", manual.ID, "", 10)
	if err != nil || len(conversations) != 0 {
		t.Fatalf("rejected conversation left a binding: %+v %v", conversations, err)
	}
	conversation, err := client.CreateConversationWithGoal(ctx, "demo", manual.ID, manualOwner, &padded)
	if err != nil || conversation == nil || conversation.TaskID != manual.ID || conversation.Owner != manualOwner {
		t.Fatalf("selected manual conversation: %+v %v", conversation, err)
	}
}
