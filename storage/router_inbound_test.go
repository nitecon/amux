package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

func inboundTestEvent(seq, at int64, room, externalID string) router.Event {
	return router.Event{
		Seq: seq, ID: "channel.message_received", Version: 1, EmittedAt: at,
		Payload: json.RawMessage(`{"channel_name":"discord","room_id":"` + room + `","message":{"id":"` + externalID + `","content":"hello","author_kind":"human"}}`),
	}
}

func TestIngestRouterInboundMessageAndUnknownRoom(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, Project{Ident: "inbound-project", ChannelName: "discord", RoomID: "room-a"}); err != nil {
		t.Fatal(err)
	}
	sub := s.EventBus().Subscribe(8)
	defer sub.Unsubscribe()
	stop, err := s.IngestRouterInbound(ctx, inboundTestEvent(4, 1234, "unknown", "ignored"))
	if err != nil || !stop {
		t.Fatalf("unknown room stop=%v err=%v", stop, err)
	}
	select {
	case event := <-sub.C:
		t.Fatalf("unknown room published event: %+v", event)
	default:
	}
	stop, err = s.IngestRouterInbound(ctx, inboundTestEvent(4, 1234, "room-a", "external-1"))
	if err != nil || stop {
		t.Fatalf("known room stop=%v err=%v", stop, err)
	}
	select {
	case event := <-sub.C:
		if event.Topic != eventbus.ProjectChanged {
			t.Fatalf("published topic = %q", event.Topic)
		}
	default:
		t.Fatal("accepted message did not publish project change")
	}
	var source, externalID, content, messageType, author string
	var sentAt int64
	var deliver bool
	var confirmed sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT source,external_message_id,content,sent_at,confirmed_at,deliver_to_agents,message_type,author_kind
		FROM messages`).Scan(&source, &externalID, &content, &sentAt, &confirmed, &deliver, &messageType, &author); err != nil {
		t.Fatal(err)
	}
	if source != "user" || externalID != "external-1" || content != "hello" || sentAt != 1234 || confirmed.Valid || deliver || messageType != "message" || author != "human" {
		t.Fatalf("stored message source=%q external=%q content=%q sent_at=%d confirmed=%v deliver=%v type=%q author=%q", source, externalID, content, sentAt, confirmed, deliver, messageType, author)
	}
	var last string
	var cursor int64
	if err := s.db.QueryRowContext(ctx, "SELECT last_msg_id FROM projects WHERE ident='inbound-project'").Scan(&last); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT event_seq FROM inbound_cursors WHERE project='inbound-project'").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if last != "external-1" || cursor != 4 {
		t.Fatalf("last_msg_id=%q cursor=%d", last, cursor)
	}
}

func TestIngestRouterInboundDeduplicatesBeforeCheckpointAndPreservesOlderReplay(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, Project{Ident: "inbound-project", ChannelName: "discord", RoomID: "room-a", LastMsgID: ptr("legacy-head")}); err != nil {
		t.Fatal(err)
	}
	sub := s.EventBus().Subscribe(8)
	defer sub.Unsubscribe()
	// A retained legacy row participates in the same project-scoped external-ID check.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO messages(project_ident,source,external_message_id,content,sent_at)
		VALUES('inbound-project','user','legacy-id','legacy',1)`); err != nil {
		t.Fatal(err)
	}
	if stop, err := s.IngestRouterInbound(ctx, inboundTestEvent(9, 90, "room-a", "newest")); err != nil || stop {
		t.Fatalf("new message stop=%v err=%v", stop, err)
	}
	if stop, err := s.IngestRouterInbound(ctx, inboundTestEvent(2, 20, "room-a", "older")); err != nil || stop {
		t.Fatalf("older replay stop=%v err=%v", stop, err)
	}
	for range 2 {
		select {
		case event := <-sub.C:
			if event.Topic != eventbus.ProjectChanged {
				t.Fatalf("published topic = %q", event.Topic)
			}
		default:
			t.Fatal("committed message did not publish project change")
		}
	}
	if stop, err := s.IngestRouterInbound(ctx, inboundTestEvent(10, 100, "room-a", "newest")); err != nil || stop {
		t.Fatalf("provider redelivery stop=%v err=%v", stop, err)
	}
	if stop, err := s.IngestRouterInbound(ctx, inboundTestEvent(10, 100, "room-a", "legacy-id")); err != nil || stop {
		t.Fatalf("legacy duplicate stop=%v err=%v", stop, err)
	}
	select {
	case event := <-sub.C:
		t.Fatalf("duplicate message published event: %+v", event)
	default:
	}
	var count int
	var cursor int64
	var last string
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE project_ident='inbound-project'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT event_seq FROM inbound_cursors WHERE project='inbound-project'").Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT last_msg_id FROM projects WHERE ident='inbound-project'").Scan(&last); err != nil {
		t.Fatal(err)
	}
	if count != 3 || cursor != 9 || last != "newest" {
		t.Fatalf("message count=%d cursor=%d last_msg_id=%q", count, cursor, last)
	}
}

func TestIngestRouterInboundFailureRollsBackWholeMessageAndCursor(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if err := s.InsertProject(ctx, Project{Ident: "inbound-project", ChannelName: "discord", RoomID: "room-a"}); err != nil {
		t.Fatal(err)
	}
	sub := s.EventBus().Subscribe(8)
	defer sub.Unsubscribe()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_inbound_project_head BEFORE UPDATE OF last_msg_id ON projects
		BEGIN SELECT RAISE(ABORT,'injected inbound head failure'); END`); err != nil {
		t.Fatal(err)
	}
	if stop, err := s.IngestRouterInbound(ctx, inboundTestEvent(1, 1, "room-a", "external")); err == nil || stop {
		t.Fatalf("failure result stop=%v err=%v", stop, err)
	}
	var messages, cursors int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM inbound_cursors").Scan(&cursors); err != nil {
		t.Fatal(err)
	}
	var last *string
	if err := s.db.QueryRowContext(ctx, "SELECT last_msg_id FROM projects WHERE ident='inbound-project'").Scan(&last); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || cursors != 0 || last != nil {
		t.Fatalf("rollback state messages=%d cursors=%d last=%v", messages, cursors, last)
	}
	select {
	case event := <-sub.C:
		t.Fatalf("failed ingestion published event: %+v", event)
	default:
	}
}

func TestIngestRouterInboundRejectsMalformedPayloadAndEvent(t *testing.T) {
	s := testStore(t)
	for _, raw := range []string{
		`null`, `{}`, `{"channel_name":null,"room_id":"r","message":{"id":"i","content":"c","author_kind":"human"}}`,
		`{"channel_name":"c","room_id":"r","message":{"id":"i","content":"c","author_kind":"human"}} {}`,
	} {
		event := inboundTestEvent(1, 1, "r", "i")
		event.Payload = json.RawMessage(raw)
		if _, err := s.IngestRouterInbound(context.Background(), event); err == nil {
			t.Errorf("accepted malformed payload %s", raw)
		}
	}
	event := inboundTestEvent(1, 1, "r", "i")
	event.Version = 2
	if _, err := s.IngestRouterInbound(context.Background(), event); err == nil {
		t.Fatal("accepted unsupported version")
	}
}
