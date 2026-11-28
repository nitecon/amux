package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/identity/agent"
)

func migrateTestAssignmentAdoption(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateAssignmentAdoption(context.Background(), tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func ownedInProgressTask(t *testing.T, s *Store, actor *agent.Origin) *Task {
	t.Helper()
	task := makeTask(t, s, actor)
	if _, err := s.TransitionTask(context.Background(), "tasks", task.ID, "in_progress", actor); err != nil {
		t.Fatal(err)
	}
	task, err := readTask(context.Background(), s.db, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestAdoptWorkerAssignmentIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET description=?,details=? WHERE id=?", "Task description", "Task details", task.ID); err != nil {
		t.Fatal(err)
	}
	task, err := readTask(ctx, s.db, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.AdoptWorkerAssignment(ctx, " TASKS ", actor)
	if err != nil || first == nil {
		t.Fatalf("adopt: %+v %v", first, err)
	}
	if first.Task.ID != task.ID || first.Conversation.TaskID != task.ID || first.Conversation.Owner != *actor {
		t.Fatalf("assignment binding %+v", first)
	}
	if _, err = uuid.Parse(first.RunID); err != nil {
		t.Fatalf("run ID %q: %v", first.RunID, err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", first.Conversation.ID, 0, 10)
	if err != nil || len(commands) != 1 || commands[0].State != "queued" || commands[0].Command.Kind != "prompt" {
		t.Fatalf("initial queued prompt %+v: %v", commands, err)
	}
	promptID := commands[0].ID
	text := commands[0].Command.Text
	for _, part := range []string{task.ID, task.Title, "evaluate this task", "perform", "ask the user"} {
		if !strings.Contains(text, part) {
			t.Fatalf("initial prompt %q omitted %q", text, part)
		}
	}
	if task.Description != nil && *task.Description != "" && !strings.Contains(text, *task.Description) {
		t.Fatalf("initial prompt omitted description: %q", text)
	}
	if task.Details != nil && *task.Details != "" && !strings.Contains(text, *task.Details) {
		t.Fatalf("initial prompt omitted details: %q", text)
	}
	second, err := s.AdoptWorkerAssignment(ctx, "tasks", actor)
	if err != nil || second == nil || second.RunID != first.RunID || second.Conversation.ID != first.Conversation.ID {
		t.Fatalf("idempotent adoption %+v: %v", second, err)
	}
	commands, err = s.ConversationCommands(ctx, "tasks", second.Conversation.ID, 0, 10)
	if err != nil || len(commands) != 1 || commands[0].ID != promptID {
		t.Fatalf("repeated adoption changed prompt set %+v: %v", commands, err)
	}
}

func TestAdoptWorkerAssignmentWithGoalValidatesScopeBeforeRetryOrCreation(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	actor := taskActor()
	taskGoal := "ship"
	task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Scoped", Reporter: "fixture", Goal: &taskGoal}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransitionTask(ctx, "tasks", task.ID, "in_progress", actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdoptWorkerAssignmentWithGoal(ctx, "tasks", actor, nil); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("missing selected goal accepted: %v", err)
	}
	wrong := "Ship"
	if _, err = s.AdoptWorkerAssignmentWithGoal(ctx, "tasks", actor, &wrong); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("case-mismatched selected goal accepted: %v", err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_assignment_runs").Scan(&count); err != nil || count != 0 {
		t.Fatalf("mismatched selection created binding count=%d err=%v", count, err)
	}
	selected := "  ship  "
	first, err := s.AdoptWorkerAssignmentWithGoal(ctx, "tasks", actor, &selected)
	if err != nil || first == nil {
		t.Fatalf("matching normalized selection: assignment=%+v err=%v", first, err)
	}
	second, err := s.AdoptWorkerAssignmentWithGoal(ctx, "tasks", actor, &taskGoal)
	if err != nil || second == nil || first.RunID != second.RunID || first.Conversation.ID != second.Conversation.ID {
		t.Fatalf("matching retry changed binding: first=%+v second=%+v err=%v", first, second, err)
	}
	if _, err = s.AdoptWorkerAssignmentWithGoal(ctx, "tasks", actor, &wrong); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("retry with wrong goal accepted: %v", err)
	}
	if _, err = s.AdoptWorkerAssignment(ctx, "tasks", actor); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("legacy nil-scope wrapper adopted tagged task on retry: %v", err)
	}
	commands, err := s.ConversationCommands(ctx, "tasks", first.Conversation.ID, 0, 10)
	if err != nil || len(commands) != 1 || commands[0].Command.Kind != "prompt" {
		t.Fatalf("adoption retry prompt count=%d commands=%+v err=%v", len(commands), commands, err)
	}
}

func TestAdoptWorkerAssignmentRequiresExactOwnerAndRejectsAmbiguity(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	actor := taskActor()
	ownedInProgressTask(t, s, actor)
	otherInstance := *actor
	otherInstance.InstanceID = uuid.NewString()
	got, err := s.AdoptWorkerAssignment(ctx, "tasks", &otherInstance)
	if err != nil || got != nil {
		t.Fatalf("different instance adopted task: %+v, %v", got, err)
	}
	ownedInProgressTask(t, s, actor)
	if _, err = s.AdoptWorkerAssignment(ctx, "tasks", actor); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("ambiguous owner adoption: %v", err)
	}
}

func TestAdoptWorkerAssignmentRollsBackOnPromptInsertFailure(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	actor := taskActor()
	ownedInProgressTask(t, s, actor)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_adoption_prompt BEFORE INSERT ON amux_conversation_commands WHEN NEW.kind='prompt' BEGIN SELECT RAISE(ABORT,'injected prompt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdoptWorkerAssignment(ctx, "tasks", actor); err == nil {
		t.Fatal("prompt insertion failure was ignored")
	}
	var count int
	for _, table := range []string{"amux_assignment_runs", "amux_conversations", "amux_conversation_commands"} {
		query := "SELECT COUNT(*) FROM " + table
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial adoption in %s: count %d, error %v", table, count, err)
		}
	}
}

func TestAdoptWorkerAssignmentBoundsEncodedPrompt(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	// Escaping makes this serialized command exceed 1 MiB even though the source
	// prompt text itself is within validateConversationCommand's text limit.
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET description=? WHERE id=?", strings.Repeat("\n", 600_000), task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdoptWorkerAssignment(ctx, "tasks", actor); !errors.Is(err, ErrConversationInput) {
		t.Fatalf("oversized encoded prompt returned %v", err)
	}
	var conversations, assignments int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_conversations").Scan(&conversations); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_assignment_runs").Scan(&assignments); err != nil {
		t.Fatal(err)
	}
	if conversations != 0 || assignments != 0 {
		t.Fatalf("oversized prompt left partial state: conversations=%d assignments=%d", conversations, assignments)
	}
}

func TestAdoptWorkerAssignmentCreatesFreshBindingForNewClaim(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	migrateTestAssignmentAdoption(t, s)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	first, err := s.AdoptWorkerAssignment(ctx, "tasks", actor)
	if err != nil || first == nil {
		t.Fatalf("first claim adoption %+v: %v", first, err)
	}
	if _, err = s.TransitionTask(ctx, "tasks", task.ID, "todo", actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TransitionTask(ctx, "tasks", task.ID, "in_progress", actor); err != nil {
		t.Fatal(err)
	}
	second, err := s.AdoptWorkerAssignment(ctx, "tasks", actor)
	if err != nil || second == nil || second.RunID == first.RunID || second.Conversation.ID == first.Conversation.ID {
		t.Fatalf("new claim reused prior adoption %+v: %v", second, err)
	}
	retry, err := s.AdoptWorkerAssignment(ctx, "tasks", actor)
	if err != nil || retry == nil || retry.RunID != second.RunID || retry.Conversation.ID != second.Conversation.ID {
		t.Fatalf("same claim retry changed binding %+v: %v", retry, err)
	}
	for _, id := range []string{first.Conversation.ID, second.Conversation.ID} {
		commands, err := s.ConversationCommands(ctx, "tasks", id, 0, 10)
		if err != nil || len(commands) != 1 || commands[0].Command.Kind != "prompt" {
			t.Fatalf("claim conversation %s prompt count %d: %v", id, len(commands), err)
		}
	}
}
