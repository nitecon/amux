package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestProjectDeletionRemovesGatewayDataAndPreservesOtherProjects(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	other := Project{Ident: "survivor", ChannelName: "internal", Kind: "adhoc"}
	if err := s.InsertProject(ctx, other); err != nil {
		t.Fatal(err)
	}
	target := makeTask(t, s, nil)
	survivor, err := s.InsertTask(ctx, TaskInsert{Project: other.Ident, Title: "Keep", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO task_subtasks(parent_id,child_id) VALUES(?,?)", target.ID, survivor.ID); err != nil {
		t.Fatal(err)
	}
	actor := taskActor()
	chat, err := s.CreateConversation(ctx, "tasks", target.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSession(ctx, "tasks", chat.ID, "native-session", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendConversationEvent(ctx, "tasks", chat.ID, uuid.NewString(), conversation.Event{
		Kind: "text_delta", SessionID: "native-session", MessageID: "example", Text: "delete me",
	}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", chat.ID, uuid.NewString(), conversation.Command{
		Kind: "prompt", Text: "delete me too",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE task_dispatches(task_id TEXT PRIMARY KEY REFERENCES tasks(id), project_ident TEXT NOT NULL);
		CREATE TABLE task_dispatch_events(task_id TEXT NOT NULL, project_ident TEXT NOT NULL);
		CREATE TABLE task_dispatch_requests(project_ident TEXT NOT NULL);
		CREATE TABLE machine_checkouts(project_ident TEXT NOT NULL);
		INSERT INTO task_dispatch_requests VALUES('tasks'),('survivor');
		INSERT INTO machine_checkouts VALUES('tasks'),('survivor');`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_dispatches VALUES(?1,'tasks'),(?2,'survivor');
		INSERT INTO task_dispatch_events VALUES(?1,'tasks'),(?2,'survivor');`, target.ID, survivor.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_events(kind,project_ident,task_json,created_at)
		VALUES('updated','tasks','{}',0)`); err != nil {
		t.Fatal(err)
	}
	cursor, err := eventTail(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteProject(ctx, "tasks", "Tasks"); !errors.Is(err, ErrProjectDeleteInput) {
		t.Fatalf("wrong confirmation returned %v", err)
	}
	deleted, err := s.DeleteProject(ctx, "tasks", "tasks")
	if err != nil || !deleted {
		t.Fatalf("delete returned %v, %v", deleted, err)
	}
	for _, check := range []struct{ query, value string }{
		{"SELECT COUNT(*) FROM projects WHERE ident=?", "tasks"},
		{"SELECT COUNT(*) FROM tasks WHERE id=?", target.ID},
		{"SELECT COUNT(*) FROM task_events WHERE project_ident=?", "tasks"},
		{"SELECT COUNT(*) FROM amux_conversation_events WHERE conversation_id=?", chat.ID},
		{"SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=?", chat.ID},
		{"SELECT COUNT(*) FROM task_subtasks WHERE parent_id=?", target.ID},
		{"SELECT COUNT(*) FROM task_dispatches WHERE project_ident=?", "tasks"},
		{"SELECT COUNT(*) FROM task_dispatch_events WHERE project_ident=?", "tasks"},
		{"SELECT COUNT(*) FROM task_dispatch_requests WHERE project_ident=?", "tasks"},
		{"SELECT COUNT(*) FROM machine_checkouts WHERE project_ident=?", "tasks"},
	} {
		var count int
		if err := s.db.QueryRowContext(ctx, check.query, check.value).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s returned %d, %v", check.query, count, err)
		}
	}
	if subscription, err := s.SubscribeTaskEvents(ctx, "after-delete", "desktop", &cursor); err != nil || subscription.Cursor != cursor {
		t.Fatalf("deleted event made its cursor unusable: %+v, %v", subscription, err)
	}
	rows, err := s.Tasks(ctx, other.Ident, []string{"todo"}, true)
	if err != nil || len(rows) != 1 || rows[0].ID != survivor.ID || rows[0].Parent != nil {
		t.Fatalf("survivor changed: %+v, %v", rows, err)
	}
	recreated := Project{Ident: "tasks", ChannelName: "internal", Kind: "adhoc"}
	if err := s.InsertProject(ctx, recreated); !errors.Is(err, ErrProjectDeleted) {
		t.Fatalf("automatic registration after deletion returned %v", err)
	}
	if err := s.RegisterProject(ctx, recreated, true); err != nil {
		t.Fatalf("explicit registration returned %v", err)
	}
	rows, err = s.Tasks(ctx, "tasks", []string{"todo", "in_progress", "done"}, true)
	if err != nil || len(rows) != 0 {
		t.Fatalf("deleted tasks reappeared: %+v, %v", rows, err)
	}
}
