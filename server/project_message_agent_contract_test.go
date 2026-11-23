package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestNativeAgentProjectMessagePreservesExplicitSourceMetadataAndRoomIdentity(t *testing.T) {
	store, handler := taskAPI(t)
	registered := taskRequest(handler, http.MethodPost, "/v1/projects", `{"ident":"native"}`, nil)
	if registered.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", registered.Code, registered.Body.String())
	}
	roomResponse := taskRequest(handler, http.MethodPost, "/v1/projects/native/room", "", nil)
	if roomResponse.Code != http.StatusOK {
		t.Fatalf("room status=%d body=%s", roomResponse.Code, roomResponse.Body.String())
	}
	var room storage.Project
	if err := json.Unmarshal(roomResponse.Body.Bytes(), &room); err != nil || room.Ident != "native" || room.RoomID != "native" || room.ChannelName != "internal" || room.Kind != "adhoc" {
		t.Fatalf("native room=%+v err=%v", room, err)
	}

	origin := apiActor()
	body := "  source body stays literal\n"
	request, err := json.Marshal(map[string]any{"body": body, "subject": "Subject", "hostname": "host", "event_at": 123})
	if err != nil {
		t.Fatal(err)
	}
	response := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages", string(request), origin)
	if response.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
	}
	var receipt struct {
		MessageID         int64  `json:"message_id"`
		ExternalMessageID string `json:"external_message_id"`
		ParentMessageID   *int64 `json:"parent_message_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.MessageID <= 0 || receipt.ParentMessageID != nil || receipt.ExternalMessageID != fmt.Sprintf("internal:%d", receipt.MessageID) {
		t.Fatalf("native receipt=%+v err=%v", receipt, err)
	}
	thread, err := store.GetProjectMessageThread(context.Background(), "native", receipt.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if thread.Root.Content != body || thread.Root.AgentID == nil || *thread.Root.AgentID != origin.SessionID || thread.Root.Subject == nil || *thread.Root.Subject != "Subject" || thread.Root.Hostname == nil || *thread.Root.Hostname != "host" || thread.Root.EventAt == nil || *thread.Root.EventAt != 123 || thread.Root.ExternalMessageID == nil || *thread.Root.ExternalMessageID != receipt.ExternalMessageID || thread.Root.ConfirmedAt == nil {
		t.Fatalf("stored native root=%+v, want literal body, sender, explicit metadata and receipt", thread.Root)
	}
	if len(thread.Confirmations) != 1 || thread.Confirmations[0].AgentID != origin.SessionID {
		t.Fatalf("sender confirmation=%+v, want session %s for message %d", thread.Confirmations, origin.SessionID, receipt.MessageID)
	}
}
