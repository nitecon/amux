package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/storage"
)

func TestProjectMessageRoutesRequireNativeOriginAndUseItsSession(t *testing.T) {
	store, handler := taskAPI(t)
	ctx := context.Background()
	if err := store.InsertProject(ctx, storage.Project{Ident: "inbox", ChannelName: "discord", RoomID: "room-a", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	event := router.Event{
		Seq: 1, ID: "channel.message_received", Version: 1, EmittedAt: 1234,
		Payload: json.RawMessage(`{"channel_name":"discord","room_id":"room-a","message":{"id":"external-1","content":"hello inbox","author_kind":"human"}}`),
	}
	if stop, err := store.IngestRouterInbound(ctx, event); err != nil || stop {
		t.Fatalf("ingest message stop=%t err=%v", stop, err)
	}

	path := "/v1/projects/inbox/messages/unread"
	withoutOrigin := taskRequest(handler, http.MethodGet, path, "", nil)
	if withoutOrigin.Code != http.StatusBadRequest {
		t.Fatalf("missing origin GET status=%d body=%s", withoutOrigin.Code, withoutOrigin.Body.String())
	}
	withoutOrigin = taskRequest(handler, http.MethodPost, "/v1/projects/inbox/messages/1/confirm", "", nil)
	if withoutOrigin.Code != http.StatusBadRequest {
		t.Fatalf("missing origin POST status=%d body=%s", withoutOrigin.Code, withoutOrigin.Body.String())
	}

	actorA := apiActor()
	actorB := *actorA
	actorB.SessionID = uuid.NewString()
	actorB.InstanceID = uuid.NewString()
	readA := taskRequest(handler, http.MethodGet, path, "", actorA)
	if readA.Code != http.StatusOK {
		t.Fatalf("read A status=%d body=%s", readA.Code, readA.Body.String())
	}
	var unread unreadProjectMessagesResponse
	if err := json.Unmarshal(readA.Body.Bytes(), &unread); err != nil || len(unread.Messages) != 1 || unread.Messages[0].ProjectIdent != "inbox" || unread.Messages[0].ID <= 0 || unread.Status != "1 unread user message(s)" {
		t.Fatalf("unread response=%+v err=%v", unread, err)
	}
	messageID := unread.Messages[0].ID

	// taskRequest supplies a legacy X-Agent-Id alias ("worker"). The session
	// origin, not that alias or any body field, owns this confirmation.
	confirmed := taskRequest(handler, http.MethodPost, "/v1/projects/inbox/messages/"+strconv.FormatInt(messageID, 10)+"/confirm", `{"agent_id":"forged"}`, actorA)
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	var ack confirmProjectMessageResponse
	if err := json.Unmarshal(confirmed.Body.Bytes(), &ack); err != nil || !ack.Confirmed {
		t.Fatalf("confirm response=%+v err=%v", ack, err)
	}
	repeat := taskRequest(handler, http.MethodPost, "/v1/projects/inbox/messages/"+strconv.FormatInt(messageID, 10)+"/confirm", "", actorA)
	if repeat.Code != http.StatusOK || json.Unmarshal(repeat.Body.Bytes(), &ack) != nil || ack.Confirmed {
		t.Fatalf("repeat confirmation status=%d response=%+v body=%s", repeat.Code, ack, repeat.Body.String())
	}
	afterA := taskRequest(handler, http.MethodGet, path, "", actorA)
	if afterA.Code != http.StatusOK || json.Unmarshal(afterA.Body.Bytes(), &unread) != nil || len(unread.Messages) != 0 || unread.Status != "no messages" {
		t.Fatalf("session A unread=%+v status=%d body=%s", unread, afterA.Code, afterA.Body.String())
	}
	afterB := taskRequest(handler, http.MethodGet, path, "", &actorB)
	if afterB.Code != http.StatusOK || json.Unmarshal(afterB.Body.Bytes(), &unread) != nil || len(unread.Messages) != 1 || unread.Messages[0].ID != messageID {
		t.Fatalf("session B inbox changed by A confirmation: unread=%+v status=%d body=%s", unread, afterB.Code, afterB.Body.String())
	}
	aliasInbox, err := store.GetUnconfirmedProjectMessages(ctx, "inbox", "worker")
	if err != nil || len(aliasInbox) != 1 || aliasInbox[0].ID != messageID {
		t.Fatalf("legacy alias was used as confirmation principal: messages=%+v err=%v", aliasInbox, err)
	}
}

func TestProjectMessageConfirmRejectsInvalidAndForeignMessageIDs(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "inbox", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	for _, id := range []string{"0", "-1", "not-an-id", "9223372036854775808"} {
		response := taskRequest(handler, http.MethodPost, "/v1/projects/inbox/messages/"+id+"/confirm", "", actor)
		if response.Code != http.StatusBadRequest {
			t.Errorf("message ID %q status=%d body=%s", id, response.Code, response.Body.String())
		}
	}
	response := taskRequest(handler, http.MethodPost, "/v1/projects/inbox/messages/1/confirm", "", actor)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing message status=%d body=%s", response.Code, response.Body.String())
	}
}
