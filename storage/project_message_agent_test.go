package storage

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

func TestCreateAgentProjectMessageStoresInternalReceiptAndSenderAtomically(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent-message.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	actor := *taskActor()
	if err := s.InsertProject(ctx, fixtureProject("native")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='internal' WHERE ident='native'"); err != nil {
		t.Fatal(err)
	}
	content := "  keep this body literally\n "
	message, err := s.CreateAgentProjectMessage(ctx, "NATIVE", actor, AgentProjectMessageInput{Content: content, MessageType: "message"})
	if err != nil {
		t.Fatal(err)
	}
	if message.ProjectIdent != "native" || message.Content != content || message.Source != "agent" || message.AgentID == nil || *message.AgentID != actor.SessionID ||
		message.MessageType != "message" || message.ParentMessageID != nil || message.DeliverToAgents || message.AuthorKind == nil || *message.AuthorKind != "agent" ||
		message.ConfirmedAt == nil || message.ExternalMessageID == nil || *message.ExternalMessageID != "internal:"+strconv.FormatInt(message.ID, 10) {
		t.Fatalf("stored internal agent message=%+v", message)
	}
	if message.Subject != nil || message.Hostname != nil || message.EventAt != nil {
		t.Fatalf("nil source metadata was defaulted by storage: %+v", message)
	}
	assertAgentMessageCounts(t, s, "native", actor.SessionID, 1, 1, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	reopened, err := readProjectMessageByID(ctx, s.db, "native", message.ID)
	if err != nil || reopened == nil || reopened.ExternalMessageID == nil || *reopened.ExternalMessageID != *message.ExternalMessageID {
		t.Fatalf("reopened internal receipt=%+v err=%v", reopened, err)
	}
	assertAgentMessageCounts(t, s, "native", actor.SessionID, 1, 1, 1)
}

func TestCreateAgentProjectMessageUsesDirectSameProjectParentAndRejectsForeignParent(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "native", "other", "external")
	for _, project := range []string{"native", "other"} {
		if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='internal' WHERE ident=?", project); err != nil {
			t.Fatal(err)
		}
	}
	actor := *taskActor()
	root, err := s.CreateAgentProjectMessage(ctx, "native", actor, AgentProjectMessageInput{Content: "root", MessageType: "message"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.CreateAgentProjectMessage(ctx, "native", actor, AgentProjectMessageInput{
		Content: "reply", ParentMessageID: &root.ID, MessageType: "reply", Subject: stringPtr(""), Hostname: stringPtr("host"), EventAt: int64Pointer(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	action, err := s.CreateAgentProjectMessage(ctx, "native", actor, AgentProjectMessageInput{
		Content: "action", ParentMessageID: &reply.ID, MessageType: "action",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.ParentMessageID == nil || *reply.ParentMessageID != root.ID || action.ParentMessageID == nil || *action.ParentMessageID != reply.ID {
		t.Fatalf("agent reply/action were normalized away from direct parents: reply=%+v action=%+v", reply, action)
	}
	if reply.Subject == nil || *reply.Subject != "" || reply.Hostname == nil || *reply.Hostname != "host" || reply.EventAt == nil || *reply.EventAt != 0 {
		t.Fatalf("supplied optional metadata not preserved: %+v", reply)
	}
	foreign, err := s.CreateAgentProjectMessage(ctx, "other", actor, AgentProjectMessageInput{Content: "foreign", MessageType: "message"})
	if err != nil {
		t.Fatal(err)
	}
	beforeMessages, beforeAgents, beforeAcks := agentMessageTableCounts(t, s, actor.SessionID)
	if _, err := s.CreateAgentProjectMessage(ctx, "native", actor, AgentProjectMessageInput{
		Content: "must not persist", ParentMessageID: &foreign.ID, MessageType: "reply",
	}); !errors.Is(err, ErrProjectMessageNotFound) {
		t.Fatalf("foreign project parent error=%v, want ErrProjectMessageNotFound", err)
	}
	afterMessages, afterAgents, afterAcks := agentMessageTableCounts(t, s, actor.SessionID)
	if beforeMessages != afterMessages || beforeAgents != afterAgents || beforeAcks != afterAcks {
		t.Fatalf("foreign parent changed rows: messages %d->%d agents %d->%d confirmations %d->%d", beforeMessages, afterMessages, beforeAgents, afterAgents, beforeAcks, afterAcks)
	}
}

func TestCreateAgentProjectMessageRejectsInvalidOriginAndExternalProjectBeforeWrites(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "native", "external")
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='internal' WHERE ident='native'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='discord' WHERE ident='external'"); err != nil {
		t.Fatal(err)
	}
	actor := *taskActor()
	input := AgentProjectMessageInput{Content: "must not persist", MessageType: "message"}
	if _, err := s.CreateAgentProjectMessage(ctx, "native", AgentOrigin{}, input); !errors.Is(err, ErrProjectMessageInput) {
		t.Fatalf("invalid origin error=%v, want ErrProjectMessageInput", err)
	}
	if _, err := s.CreateAgentProjectMessage(ctx, "external", actor, input); !errors.Is(err, ErrProjectMessageUnsupportedChannel) {
		t.Fatalf("external project error=%v, want ErrProjectMessageUnsupportedChannel", err)
	}
	for _, project := range []string{"native", "external"} {
		var messages, agents, confirmations int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE project_ident=?", project).Scan(&messages); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE project_ident=? AND agent_id=?", project, actor.SessionID).Scan(&agents); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE project_ident=? AND agent_id=?", project, actor.SessionID).Scan(&confirmations); err != nil {
			t.Fatal(err)
		}
		if messages != 0 || agents != 0 || confirmations != 0 {
			t.Fatalf("rejected write changed %s rows: messages=%d agents=%d confirmations=%d", project, messages, agents, confirmations)
		}
	}
}

func TestCreateAgentProjectMessageChecksReplyParentAndBodyBeforeChannel(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "native", "external")
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='internal' WHERE ident='native'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='discord' WHERE ident='external'"); err != nil {
		t.Fatal(err)
	}
	actor := *taskActor()
	parent, err := s.CreateAgentProjectMessage(ctx, "native", actor, AgentProjectMessageInput{Content: "parent", MessageType: "message"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgentProjectMessage(ctx, "external", actor, AgentProjectMessageInput{
		Content: "  ", ParentMessageID: &parent.ID, MessageType: "reply",
	}); !errors.Is(err, ErrProjectMessageNotFound) {
		t.Fatalf("foreign parent before blank body/channel error=%v, want not found", err)
	}
	if _, err := s.CreateAgentProjectMessage(ctx, "external", actor, AgentProjectMessageInput{
		Content: "  ", MessageType: "message",
	}); !errors.Is(err, ErrProjectMessageInput) {
		t.Fatalf("blank body before channel error=%v, want invalid input", err)
	}
	if _, err := s.CreateAgentProjectMessage(ctx, "external", actor, AgentProjectMessageInput{
		Content: "valid body", MessageType: "message",
	}); !errors.Is(err, ErrProjectMessageUnsupportedChannel) {
		t.Fatalf("valid body external channel error=%v, want unsupported channel", err)
	}
	assertAgentMessageCounts(t, s, "external", actor.SessionID, 0, 0, 0)
}

func TestCreateAgentProjectMessageReceiptFailureRollsBackAndExternalIDSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent-message-rollback.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	actor := *taskActor()
	if err := s.InsertProject(ctx, fixtureProject("native")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='internal' WHERE ident='native'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_agent_receipt BEFORE UPDATE OF external_message_id ON messages
		BEGIN SELECT RAISE(ABORT,'injected internal receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAgentProjectMessage(ctx, "native", actor, AgentProjectMessageInput{Content: "rollback", MessageType: "message"}); err == nil {
		t.Fatal("receipt update failure was ignored")
	}
	assertAgentMessageCounts(t, s, "native", actor.SessionID, 0, 0, 0)
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_agent_receipt"); err != nil {
		t.Fatal(err)
	}
	externalID := "provider-message-42"
	message, err := s.CreateAgentProjectMessage(ctx, "native", actor, AgentProjectMessageInput{
		Content: "stored receipt", ExternalMessageID: &externalID, MessageType: "message",
	})
	if err != nil || message.ExternalMessageID == nil || *message.ExternalMessageID != externalID {
		t.Fatalf("external receipt message=%+v err=%v", message, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	reopened, err := readProjectMessageByID(ctx, s.db, "native", message.ID)
	if err != nil || reopened == nil || reopened.ExternalMessageID == nil || *reopened.ExternalMessageID != externalID {
		t.Fatalf("reopened external receipt=%+v err=%v", reopened, err)
	}
	assertAgentMessageCounts(t, s, "native", actor.SessionID, 1, 1, 1)
}

func assertAgentMessageCounts(t *testing.T, s *Store, project, agentID string, messages, agents, acknowledgements int) {
	t.Helper()
	var actualMessages, actualAgents, actualAcknowledgements int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM messages WHERE project_ident=? AND agent_id=?", project, agentID).Scan(&actualMessages); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM agents WHERE project_ident=? AND agent_id=?", project, agentID).Scan(&actualAgents); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM agent_confirmations WHERE project_ident=? AND agent_id=?", project, agentID).Scan(&actualAcknowledgements); err != nil {
		t.Fatal(err)
	}
	if actualMessages != messages || actualAgents != agents || actualAcknowledgements != acknowledgements {
		t.Fatalf("agent message rows messages=%d agents=%d acknowledgements=%d, want %d/%d/%d", actualMessages, actualAgents, actualAcknowledgements, messages, agents, acknowledgements)
	}
}

func agentMessageTableCounts(t *testing.T, s *Store, agentID string) (messages, agents, confirmations int) {
	t.Helper()
	for query, target := range map[string]*int{
		"SELECT COUNT(*) FROM messages WHERE agent_id=?":            &messages,
		"SELECT COUNT(*) FROM agents WHERE agent_id=?":              &agents,
		"SELECT COUNT(*) FROM agent_confirmations WHERE agent_id=?": &confirmations,
	} {
		if err := s.db.QueryRowContext(context.Background(), query, agentID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	return messages, agents, confirmations
}
