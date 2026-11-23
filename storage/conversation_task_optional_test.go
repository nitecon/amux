package storage

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestTasklessConversationRequiresExistingProjectAndListsByProject(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	actor := taskActor()
	if _, err := s.CreateConversation(ctx, "missing", "", actor); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing project error=%v, want ErrTaskNotFound", err)
	}
	conversation, err := s.CreateConversation(ctx, "tasks", "", actor)
	if err != nil || conversation.TaskID != "" {
		t.Fatalf("taskless conversation=%+v err=%v", conversation, err)
	}
	loaded, err := s.Conversation(ctx, "tasks", conversation.ID)
	if err != nil || loaded.TaskID != "" {
		t.Fatalf("loaded taskless conversation=%+v err=%v", loaded, err)
	}
	listed, err := s.ProjectConversations(ctx, "tasks", "", 10)
	if err != nil || len(listed) != 1 || listed[0].ID != conversation.ID || listed[0].TaskID != "" {
		t.Fatalf("project conversations=%+v err=%v", listed, err)
	}
	var tasks int
	if err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("taskless create inserted task: count=%d err=%v", tasks, err)
	}
}

func TestConversationTaskOptionalMigrationPreservesHistoryAndChildren(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.InsertProject(ctx, fixtureProject("tasks")); err != nil {
		t.Fatal(err)
	}
	task := makeTask(t, s, nil)
	actor := taskActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "migration-session", actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), conversation.Event{Kind: "text_delta", SessionID: "migration-session", Text: "retained"}, actor); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `CREATE TRIGGER conversation_task_optional_fixture AFTER UPDATE OF task_id ON amux_conversations
		BEGIN INSERT OR REPLACE INTO settings(key,value) VALUES('conversation-task-linked',NEW.task_id); END`); err != nil {
		t.Fatal(err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ddl string
	if err = conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='amux_conversations'").Scan(&ddl); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	legacyPattern := regexp.MustCompile(`(?i)(\btask_id\s+TEXT)(\s+REFERENCES\s+tasks\s*\(\s*id\s*\))`)
	if len(legacyPattern.FindAllStringIndex(ddl, -1)) != 1 {
		conn.Close()
		t.Fatalf("could not construct legacy schema from %q", ddl)
	}
	legacyDDL := legacyPattern.ReplaceAllString(ddl, "$1 NOT NULL$2")
	legacyDDL, err = replaceCreateTableName(legacyDDL, "amux_conversations", "amux_conversations_legacy_fixture")
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	err = rebuildLegacyTable(ctx, conn, "amux_conversations", "amux_conversations_legacy_fixture", legacyDDL)
	if closeErr := conn.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.Conversation(ctx, "tasks", c.ID)
	if err != nil || loaded.TaskID != task.ID || loaded.NativeSessionID != "migration-session" {
		t.Fatalf("migrated conversation=%+v err=%v", loaded, err)
	}
	events, err := reopened.ConversationEvents(ctx, "tasks", c.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Event.Text != "retained" {
		t.Fatalf("migrated events=%+v err=%v", events, err)
	}
	if notNull, err := conversationTaskIDNotNull(ctx, reopened.db); err != nil || notNull {
		t.Fatalf("migrated task_id not-null=%t err=%v", notNull, err)
	}
	var fkViolations int
	if err = reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&fkViolations); err != nil || fkViolations != 0 {
		t.Fatalf("foreign key violations=%d err=%v", fkViolations, err)
	}
	var triggerCount int
	if err = reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='conversation_task_optional_fixture'").Scan(&triggerCount); err != nil || triggerCount != 1 {
		t.Fatalf("preserved trigger count=%d err=%v", triggerCount, err)
	}
	if _, err = reopened.CreateConversation(ctx, "tasks", "", actor); err != nil {
		t.Fatalf("taskless insert after migration: %v", err)
	}
}
