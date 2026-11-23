package storage

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/nitecon/amux/eventbus"
)

func TestCreateHumanProjectMessageRootReplyResolutionAndNativeInbox(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "one", "two")
	changes := s.bus.Subscribe(4)
	defer changes.Unsubscribe()
	root, err := s.CreateHumanProjectMessage(ctx, "ONE", HumanProjectMessageInput{Content: "  root question  \n"})
	if err != nil {
		t.Fatal(err)
	}
	if root.ProjectIdent != "one" || root.Content != "root question" || root.Source != "user" || root.MessageType != "message" ||
		root.ParentMessageID != nil || root.AgentID != nil || root.AuthorKind == nil || *root.AuthorKind != "human" || !root.DeliverToAgents || root.ConfirmedAt != nil {
		t.Fatalf("root fields do not match human inbox contract: %+v", root)
	}
	select {
	case event := <-changes.C:
		if event.Topic != eventbus.ProjectChanged {
			t.Fatalf("post-commit event topic=%q", event.Topic)
		}
	default:
		t.Fatal("successful human message did not publish ProjectChanged")
	}
	unread, err := s.GetUnconfirmedProjectMessages(ctx, "one", "native-session")
	if err != nil || len(unread) != 1 || unread[0].ID != root.ID {
		t.Fatalf("native inbox did not receive the human root: %+v err=%v", unread, err)
	}
	if confirmed, err := s.ConfirmProjectMessage(ctx, "one", "native-session", root.ID); err != nil || !confirmed {
		t.Fatalf("native root confirmation=%t err=%v", confirmed, err)
	}

	reply, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: " reply text ", ParentMessageID: int64Pointer(root.ID), Resolve: true, ResolvedBy: "user:alice"})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "reply text" || reply.MessageType != "reply" || reply.ParentMessageID == nil || *reply.ParentMessageID != root.ID || reply.Source != "user" || reply.AuthorKind == nil || *reply.AuthorKind != "human" {
		t.Fatalf("reply did not attach to thread root: %+v", reply)
	}
	storedRoot := readAuthoredProjectMessage(t, s, "one", root.ID)
	if storedRoot.ResolvedAt == nil || storedRoot.ResolvedBy == nil || *storedRoot.ResolvedBy != "user:alice" {
		t.Fatalf("resolve-after did not update root: %+v", storedRoot)
	}
	unread, err = s.GetUnconfirmedProjectMessages(ctx, "one", "native-session")
	if err != nil || len(unread) != 1 || unread[0].ID != reply.ID {
		t.Fatalf("native session confirmation leaked to reply: %+v err=%v", unread, err)
	}
	other, err := s.GetUnconfirmedProjectMessages(ctx, "two", "native-session")
	if err != nil || len(other) != 0 {
		t.Fatalf("human message crossed project inbox boundary: %+v err=%v", other, err)
	}

	childReply, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: " nested reply ", ParentMessageID: int64Pointer(reply.ID)})
	if err != nil || childReply.ParentMessageID == nil || *childReply.ParentMessageID != root.ID {
		t.Fatalf("reply-to-reply did not pin to root: %+v err=%v", childReply, err)
	}
}

func TestCreateHumanProjectMessageRejectsMissingAndCrossProjectParents(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "one", "two")
	root, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: "root"})
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []*int64{int64Pointer(root.ID), int64Pointer(999999)} {
		if _, err := s.CreateHumanProjectMessage(ctx, "two", HumanProjectMessageInput{Content: "must not persist", ParentMessageID: parent, Resolve: true}); !errors.Is(err, ErrProjectMessageNotFound) {
			t.Fatalf("invalid project parent error=%v", err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid parent created a message: count=%d err=%v", count, err)
	}
	if _, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: " \n\t "}); !errors.Is(err, ErrProjectMessageInput) {
		t.Fatalf("blank content error=%v", err)
	}
	if _, err := s.CreateHumanProjectMessage(ctx, "missing", HumanProjectMessageInput{Content: "valid"}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing project error=%v", err)
	}
}

func TestCreateHumanProjectMessageFallsBackToNewRootForOrphanedReply(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "one", "two")
	foreignRoot, err := s.CreateHumanProjectMessage(ctx, "two", HumanProjectMessageInput{Content: "private other-project root"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO messages(project_ident,source,content,sent_at,parent_message_id,deliver_to_agents,message_type)
		VALUES('one','user','orphaned reply',1,?,1,'reply')`, foreignRoot.ID)
	if err != nil {
		t.Fatal(err)
	}
	orphanID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: "standalone after orphan", ParentMessageID: int64Pointer(orphanID)})
	if err != nil {
		t.Fatal(err)
	}
	if created.ParentMessageID != nil || created.MessageType != "message" || created.ProjectIdent != "one" {
		t.Fatalf("dangling second-hop parent did not fall back to a new local root: %+v", created)
	}
	if created.Content == foreignRoot.Content || created.ID == foreignRoot.ID {
		t.Fatalf("thread fallback exposed a foreign project record: %+v", created)
	}
}

func TestCreateHumanProjectMessageRollsBackReplyOnResolveOrInsertFailure(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "one")
	root, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_human_message_resolve BEFORE UPDATE OF resolved_at ON messages
		WHEN OLD.id=`+strconv.FormatInt(root.ID, 10)+` BEGIN SELECT RAISE(ABORT,'forced resolve failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: "reply", ParentMessageID: int64Pointer(root.ID), Resolve: true, ResolvedBy: "user:alice"}); err == nil {
		t.Fatal("expected resolve failure")
	}
	assertAuthoredMessageCountAndRootOpen(t, s, root.ID)
	if _, err := s.db.ExecContext(ctx, "DROP TRIGGER fail_human_message_resolve"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_human_message_insert BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT,'forced insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: "reply", ParentMessageID: int64Pointer(root.ID), Resolve: true, ResolvedBy: "user:alice"}); err == nil {
		t.Fatal("expected insert failure")
	}
	assertAuthoredMessageCountAndRootOpen(t, s, root.ID)
}

func readAuthoredProjectMessage(t *testing.T, s *Store, project string, id int64) ProjectMessage {
	t.Helper()
	message, err := scanProjectMessage(s.db.QueryRowContext(context.Background(), `SELECT id,project_ident,source,external_message_id,content,sent_at,
		confirmed_at,parent_message_id,agent_id,message_type,subject,hostname,event_at,deliver_to_agents,author_kind,resolved_at,resolved_by
		FROM messages WHERE project_ident=? AND id=?`, project, id))
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func assertAuthoredMessageCountAndRootOpen(t *testing.T, s *Store, rootID int64) {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM messages").Scan(&count); err != nil || count != 1 {
		t.Fatalf("transaction left a partial reply: count=%d err=%v", count, err)
	}
	root := readAuthoredProjectMessage(t, s, "one", rootID)
	if root.ResolvedAt != nil || root.ResolvedBy != nil {
		t.Fatalf("transaction partially resolved root: %+v", root)
	}
}
