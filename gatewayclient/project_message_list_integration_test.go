package gatewayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestHumanMessageThreadListRoundTripWithUnavailableChannel(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "human-message-list.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "external", ChannelName: "discord", RoomID: "room-a", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(store, "message-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}

	root, err := client.PostHumanProjectMessage(ctx, "external", HumanProjectMessageInput{Content: "Please look at the widget", Author: "Display only"})
	if err != nil || root.MessageID <= 0 || root.ParentMessageID != nil || root.Delivered || root.DeliveryError == nil {
		t.Fatalf("root receipt=%+v err=%v", root, err)
	}
	reply, err := client.PostHumanProjectMessage(ctx, "external", HumanProjectMessageInput{Content: "Never mind, done.", ParentMessageID: &root.MessageID, Resolve: true})
	if err != nil || reply.MessageID <= root.MessageID || reply.ParentMessageID == nil || *reply.ParentMessageID != root.MessageID || reply.Delivered || reply.DeliveryError == nil {
		t.Fatalf("reply receipt=%+v err=%v", reply, err)
	}

	list, err := client.ListProjectMessageThreads(ctx, "external", ProjectMessageListQuery{State: "all"})
	if err != nil || len(list.Threads) != 1 || list.Threads[0].ID != root.MessageID || list.Threads[0].AuthorKind != "human" || list.Threads[0].State != "resolved" || list.Threads[0].ReplyCount != 1 || list.Counts.Resolved != 1 {
		t.Fatalf("listed thread=%+v err=%v", list, err)
	}

	for _, actor := range []storage.AgentOrigin{projectMessageActor(), projectMessageActor()} {
		unread, err := client.UnreadProjectMessages(ctx, "external", actor)
		if err != nil || len(unread.Messages) != 2 || unread.Messages[0].ID != root.MessageID || unread.Messages[1].ID != reply.MessageID {
			t.Fatalf("session %s unread=%+v err=%v", actor.SessionID, unread, err)
		}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+"/v1/projects/external/messages/human", strings.NewReader(`{"content":"   "}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer message-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty human message status=%d", response.StatusCode)
	}
}
