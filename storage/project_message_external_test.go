package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestExternalAgentProjectMessageRequiresAndPersistsOpaqueProviderReceipt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "external-agent-message.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("external")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='discord',room_id='' WHERE ident='external'"); err != nil {
		t.Fatal(err)
	}
	origin := *taskActor()
	for _, receipt := range []*string{nil, stringPtr(" \t ")} {
		if _, err := s.CreateAgentProjectMessage(ctx, "external", origin, AgentProjectMessageInput{
			Content: "already sent", MessageType: "message", ExternalMessageID: receipt,
		}); !errors.Is(err, ErrProjectMessageUnsupportedChannel) {
			t.Fatalf("external message without nonblank provider receipt error=%v", err)
		}
	}
	assertAgentMessageCounts(t, s, "external", origin.SessionID, 0, 0, 0)

	receipt := "discord:thread/opaque#0007"
	message, err := s.CreateAgentProjectMessage(ctx, "external", origin, AgentProjectMessageInput{
		Content: "already sent", MessageType: "message", ExternalMessageID: &receipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if message.ExternalMessageID == nil || *message.ExternalMessageID != receipt || message.Source != "agent" ||
		message.AgentID == nil || *message.AgentID != origin.SessionID || message.ConfirmedAt == nil ||
		message.AuthorKind == nil || *message.AuthorKind != "agent" {
		t.Fatalf("external agent message receipt/acknowledgement=%+v", message)
	}
	assertAgentMessageCounts(t, s, "external", origin.SessionID, 1, 1, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	reopened, err := s.ProjectMessage(ctx, "EXTERNAL", message.ID)
	if err != nil || reopened.ExternalMessageID == nil || *reopened.ExternalMessageID != receipt {
		t.Fatalf("reopened provider receipt=%+v err=%v", reopened, err)
	}
	assertAgentMessageCounts(t, s, "external", origin.SessionID, 1, 1, 1)
}

func TestExternalAgentReplyRejectsForeignParentBeforeReceiptOrWrites(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "native", "external")
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='internal' WHERE ident='native'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET channel_name='discord' WHERE ident='external'"); err != nil {
		t.Fatal(err)
	}
	origin := *taskActor()
	parent, err := s.CreateAgentProjectMessage(ctx, "native", origin, AgentProjectMessageInput{
		Content: "native parent", MessageType: "message",
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := "provider-receipt"
	if _, err := s.CreateAgentProjectMessage(ctx, "external", origin, AgentProjectMessageInput{
		Content: "external reply", ParentMessageID: &parent.ID, MessageType: "reply", ExternalMessageID: &receipt,
	}); !errors.Is(err, ErrProjectMessageNotFound) {
		t.Fatalf("foreign parent error=%v, want ErrProjectMessageNotFound", err)
	}
	assertAgentMessageCounts(t, s, "external", origin.SessionID, 0, 0, 0)
}

func TestProjectMessageLookupAndRoomPersistenceAreProjectScoped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "external-message-room.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"external", "other"} {
		if err := s.InsertProject(ctx, fixtureProject(project)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := s.CreateHumanProjectMessage(ctx, "external", HumanProjectMessageInput{Content: "root"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.CreateHumanProjectMessage(ctx, "external", HumanProjectMessageInput{Content: "reply", ParentMessageID: &root.ID})
	if err != nil {
		t.Fatal(err)
	}
	addressed, err := s.ProjectMessage(ctx, "external", reply.ID)
	if err != nil || addressed.ID != reply.ID || addressed.ParentMessageID == nil || *addressed.ParentMessageID != root.ID {
		t.Fatalf("direct addressed message=%+v err=%v", addressed, err)
	}
	if _, err := s.ProjectMessage(ctx, "other", reply.ID); !errors.Is(err, ErrProjectMessageNotFound) {
		t.Fatalf("cross-project message lookup error=%v", err)
	}
	room := "provider-room:opaque/123"
	project, err := s.SetProjectRoom(ctx, "EXTERNAL", room)
	if err != nil || project.RoomID != room || project.ChannelName != "test" {
		t.Fatalf("updated project room=%+v err=%v", project, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	project, err = s.Project(ctx, "external")
	if err != nil || project == nil || project.RoomID != room {
		t.Fatalf("reopened project room=%+v err=%v", project, err)
	}
}

func TestSetProjectMessageExternalIDScopesExistingHumanMessage(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "one", "two")
	message, err := s.CreateHumanProjectMessage(ctx, "one", HumanProjectMessageInput{Content: "sent to provider"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := "provider:message#opaque"
	if err := s.SetProjectMessageExternalID(ctx, "one", message.ID, receipt); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ProjectMessage(ctx, "one", message.ID)
	if err != nil || stored.ExternalMessageID == nil || *stored.ExternalMessageID != receipt {
		t.Fatalf("stored external receipt=%+v err=%v", stored, err)
	}
	if err := s.SetProjectMessageExternalID(ctx, "two", message.ID, "wrong-project"); !errors.Is(err, ErrProjectMessageNotFound) {
		t.Fatalf("foreign project receipt update error=%v", err)
	}
	if err := s.SetProjectMessageExternalID(ctx, "one", message.ID+100, "missing-row"); !errors.Is(err, ErrProjectMessageNotFound) {
		t.Fatalf("missing message receipt update error=%v", err)
	}
	if err := s.SetProjectMessageExternalID(ctx, "one", message.ID, "  "); !errors.Is(err, ErrProjectMessageInput) {
		t.Fatalf("blank provider receipt error=%v", err)
	}
	stored, err = s.ProjectMessage(ctx, "one", message.ID)
	if err != nil || stored.ExternalMessageID == nil || *stored.ExternalMessageID != receipt {
		t.Fatalf("rejected receipt updates changed row=%+v err=%v", stored, err)
	}
}
