package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestPostHumanProjectMessageStoresBeforeUnavailableChannelDelivery(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "external", ChannelName: "discord", RoomID: "room-a", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	response := taskRequest(handler, http.MethodPost, "/v1/projects/external/messages/human", `{"content":"  please review  ","author":"Alice"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("post status=%d body=%s", response.Code, response.Body.String())
	}
	var ack humanProjectMessageResponse
	if err := json.Unmarshal(response.Body.Bytes(), &ack); err != nil || ack.MessageID <= 0 || ack.ParentMessageID != nil || ack.Delivered || ack.DeliveryError == nil || *ack.DeliveryError != "channel plugin 'discord' is not configured" {
		t.Fatalf("post receipt=%+v err=%v", ack, err)
	}
	unread, err := store.GetUnconfirmedProjectMessages(context.Background(), "external", "native-session")
	if err != nil || len(unread) != 1 {
		t.Fatalf("stored unread=%+v err=%v", unread, err)
	}
	message := unread[0]
	if message.ID != ack.MessageID || message.Content != "please review" || message.Source != "user" || message.AuthorKind == nil || *message.AuthorKind != "human" || message.AgentID != nil || !message.DeliverToAgents || message.MessageType != "message" {
		t.Fatalf("stored human message=%+v", message)
	}
}

func TestPostHumanProjectMessageInternalThreadAndResolveAliases(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "native", ChannelName: "internal", RoomID: "native", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	rootResponse := taskRequest(handler, http.MethodPost, "/v1/projects/native/messages/human", `{"content":" root ","resolve":true,"author":"Display only"}`, nil)
	if rootResponse.Code != http.StatusOK {
		t.Fatalf("root status=%d body=%s", rootResponse.Code, rootResponse.Body.String())
	}
	var root humanProjectMessageResponse
	if err := json.Unmarshal(rootResponse.Body.Bytes(), &root); err != nil || root.MessageID <= 0 || root.ParentMessageID != nil || !root.Delivered || root.DeliveryError != nil {
		t.Fatalf("root response=%+v err=%v", root, err)
	}
	// Browser forms submit a decimal string for the parent and "on" for resolve.
	replyRequest := httptest.NewRequest(http.MethodPost, "/v1/projects/native/messages/human", strings.NewReader(`{"content":"reply","parent_message_id":"`+strconv.FormatInt(root.MessageID, 10)+`","resolve":"on"}`))
	replyRequest.Header.Set("Authorization", "Bearer task-key")
	replyRequest.Header.Set("X-User-Name", "attacker")
	replyResponse := httptest.NewRecorder()
	handler.ServeHTTP(replyResponse, replyRequest)
	if replyResponse.Code != http.StatusOK {
		t.Fatalf("reply status=%d body=%s", replyResponse.Code, replyResponse.Body.String())
	}
	var reply humanProjectMessageResponse
	if err := json.Unmarshal(replyResponse.Body.Bytes(), &reply); err != nil || reply.MessageID <= root.MessageID || reply.ParentMessageID == nil || *reply.ParentMessageID != root.MessageID || !reply.Delivered || reply.DeliveryError != nil {
		t.Fatalf("reply response=%+v err=%v", reply, err)
	}
	unread, err := store.GetUnconfirmedProjectMessages(context.Background(), "native", "native-session")
	if err != nil || len(unread) != 2 {
		t.Fatalf("thread messages=%+v err=%v", unread, err)
	}
	if unread[0].MessageType != "message" || unread[0].ParentMessageID != nil || unread[0].ResolvedAt == nil || unread[0].ResolvedBy == nil || *unread[0].ResolvedBy != "user" {
		t.Fatalf("root message resolution/provenance=%+v", unread[0])
	}
	if unread[1].MessageType != "reply" || unread[1].ParentMessageID == nil || *unread[1].ParentMessageID != root.MessageID || unread[1].AgentID != nil || unread[1].AuthorKind == nil || *unread[1].AuthorKind != "human" {
		t.Fatalf("reply message author/thread metadata=%+v", unread[1])
	}
	// A caller-supplied user header cannot change the server-owned resolver.
	if *unread[0].ResolvedBy == "user:attacker" {
		t.Fatal("caller-controlled user identity was used as resolver")
	}
}

func TestPostHumanProjectMessageRejectsUnauthenticatedAndNativeImpersonation(t *testing.T) {
	_, handler := taskAPI(t)
	path := "/v1/projects/tasks/messages/human"
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"content":"hello"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", response.Code, response.Body.String())
	}
	for _, headers := range []map[string]string{
		{"X-Agent-Session-Id": apiActor().SessionID, "X-Agent-Instance-Id": apiActor().InstanceID, "X-Agent-Provider": "codex", "X-Agent-Os": "linux"},
		{"X-Agent-Session-Id": apiActor().SessionID},
		{"X-Agent-Id": "worker"},
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"content":"hello"}`))
		req.Header.Set("Authorization", "Bearer task-key")
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Errorf("native identity headers %v returned %d: %s", headers, res.Code, res.Body.String())
		}
	}
	spoof := taskRequest(handler, http.MethodPost, path, `{"content":"hello","agent_id":"gateway-user"}`, nil)
	if spoof.Code != http.StatusBadRequest {
		t.Fatalf("spoofed agent_id status=%d body=%s", spoof.Code, spoof.Body.String())
	}
}

func TestHumanProjectMessageScalarCompatibility(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{`12`, 12}, {`"12"`, 12}, {`null`, 0}, {`0`, 0}, {`-3`, 0}, {`"bad"`, 0}, {`true`, 0}, {`1.5`, 0},
	} {
		got := humanParentMessageID(json.RawMessage(tc.value))
		if tc.want == 0 && got != nil || tc.want > 0 && (got == nil || *got != tc.want) {
			t.Errorf("parent %s => %v, want %d", tc.value, got, tc.want)
		}
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{`true`, true}, {`false`, false}, {`"on"`, true}, {`"TRUE"`, true}, {`"1"`, true}, {`"yes"`, true},
		{`1`, true}, {`-1`, true}, {`0`, false}, {`1.5`, false}, {`null`, false}, {`"no"`, false}, {`{}`, false},
	} {
		if got := humanResolveValue(json.RawMessage(tc.value)); got != tc.want {
			t.Errorf("resolve %s => %t, want %t", tc.value, got, tc.want)
		}
	}
}

func TestPostHumanProjectMessageRejectsEmptyAndForeignParent(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "other", ChannelName: "internal", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	foreign := taskRequest(handler, http.MethodPost, "/v1/projects/other/messages/human", `{"content":"foreign root"}`, nil)
	if foreign.Code != http.StatusOK {
		t.Fatalf("create foreign root status=%d body=%s", foreign.Code, foreign.Body.String())
	}
	var foreignAck humanProjectMessageResponse
	if err := json.Unmarshal(foreign.Body.Bytes(), &foreignAck); err != nil || foreignAck.MessageID <= 0 {
		t.Fatalf("foreign root receipt=%+v err=%v", foreignAck, err)
	}
	for _, body := range []string{`{"content":"  "}`, `{"content":"reply","parent_message_id":` + strconv.FormatInt(foreignAck.MessageID, 10) + `}`} {
		response := taskRequest(handler, http.MethodPost, "/v1/projects/tasks/messages/human", body, nil)
		want := http.StatusBadRequest
		if strings.Contains(body, "parent_message_id") {
			want = http.StatusNotFound
		}
		if response.Code != want {
			t.Errorf("body=%s status=%d want=%d: %s", body, response.Code, want, response.Body.String())
		}
	}
}
