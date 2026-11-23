package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
)

func TestCommandEnqueueSharesCallerEffectTransactionWithoutPrematureWake(t *testing.T) {
	s, c, _ := commandConversation(t)
	ctx := context.Background()
	if _, err := s.db.Exec("CREATE TABLE fixture_notification_receipts(uid TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	sub := s.EventBus().Subscribe(4)
	defer sub.Unsubscribe()
	id := uuid.NewString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	command, err := enqueueConversationCommandIn(ctx, tx, c.ProjectIdent, c.ID, id, conversation.Command{Kind: "prompt", Text: "A transactional notice"})
	if err != nil || command.ID != id {
		t.Fatalf("enqueue inside transaction: %+v %v", command, err)
	}
	if _, err := tx.Exec("INSERT INTO fixture_notification_receipts(uid) VALUES(NULL)"); err == nil {
		t.Fatal("notification effect failure fixture did not fail")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_command_changes", int64(0))
	select {
	case event := <-sub.C:
		t.Fatalf("rolled-back command was announced: %+v", event)
	default:
	}
	accepted, err := s.EnqueueConversationCommand(ctx, c.ProjectIdent, c.ID, id, conversation.Command{Kind: "prompt", Text: "A transactional notice"})
	if err != nil || accepted.ID != id || accepted.Sequence != 1 {
		t.Fatalf("retry after rollback: %+v %v", accepted, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_command_changes", int64(1))
}
