package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type sdkExternalMessageProvider struct{}

func (sdkExternalMessageProvider) EnsureRoom(context.Context, string) (string, error) {
	return "external-room", nil
}

func (sdkExternalMessageProvider) Send(_ context.Context, _ string, parent *string, _ channels.OutboundMessage) (string, error) {
	if parent == nil {
		return "opaque-provider-root", nil
	}
	if *parent != "opaque-provider-root" {
		return "", fmt.Errorf("unexpected provider parent %q", *parent)
	}
	return "opaque-provider-reply", nil
}

func (sdkExternalMessageProvider) SendText(context.Context, string, *string, string) (string, error) {
	return "opaque-provider-human", nil
}

func TestAgentProjectMessageRequestsUseNativeOriginAndExactReceipts(t *testing.T) {
	actor := projectMessageActor()
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Agent-Session-Id") != actor.SessionID || r.Header.Get("X-Agent-Instance-Id") != actor.InstanceID || r.Header.Get("X-Agent-Provider") != actor.Provider || r.Header.Get("X-Agent-OS") != actor.OS || r.Header.Get("X-Agent-Id") != "" {
			t.Errorf("message identity headers=%v", r.Header)
		}
		switch r.URL.Path {
		case "/v1/projects/demo/messages":
			if r.Method != http.MethodPost {
				t.Errorf("root method=%s", r.Method)
			}
			_, _ = fmt.Fprint(w, `{"message_id":40,"external_message_id":"internal:40"}`)
		case "/v1/projects/demo/messages/50/reply":
			_, _ = fmt.Fprint(w, `{"message_id":51,"external_message_id":"internal:51","parent_message_id":50}`)
		case "/v1/projects/demo/messages/50/action":
			_, _ = fmt.Fprint(w, `{"message_id":52,"external_message_id":"provider-52","parent_message_id":50}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, content, message := "literal body", "alias", "action body"
	root, err := client.SendProjectMessage(context.Background(), "DEMO", ProjectMessageInput{Body: &body, Content: &content}, actor)
	if err != nil || root.MessageID != 40 || root.ExternalMessageID != "internal:40" || root.ParentMessageID != nil {
		t.Fatalf("root receipt=%+v err=%v", root, err)
	}
	parent := int64(50)
	reply, err := client.ReplyToProjectMessage(context.Background(), "demo", parent, ProjectMessageInput{Body: &body}, actor)
	if err != nil || reply.MessageID != 51 || reply.ParentMessageID == nil || *reply.ParentMessageID != parent {
		t.Fatalf("reply receipt=%+v err=%v", reply, err)
	}
	action, err := client.TakeProjectMessageAction(context.Background(), "demo", parent, ProjectMessageInput{Message: &message}, actor)
	if err != nil || action.MessageID != 52 || action.ParentMessageID == nil || *action.ParentMessageID != parent {
		t.Fatalf("action receipt=%+v err=%v", action, err)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests=%d, want one per write", requests.Load())
	}
}

func TestAgentProjectMessageRejectsMalformedReceiptsWithoutRetry(t *testing.T) {
	actor := projectMessageActor()
	for _, response := range []string{
		`{"message_id":40}`, // missing receipt ID
		`{"message_id":40,"external_message_id":""}`,
		`{"message_id":40,"external_message_id":"internal:41"}`,
		`{"message_id":40,"external_message_id":"internal:40","parent_message_id":null}`,
		`{"message_id":50,"external_message_id":"internal:50","parent_message_id":50}`,
		`{"message_id":51,"external_message_id":"internal:51","parent_message_id":49}`,
	} {
		t.Run(response, func(t *testing.T) {
			var requests atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				_, _ = fmt.Fprint(w, response)
			}))
			defer host.Close()
			client, err := New(host.URL, "message-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			body := "write once"
			parent := int64(50)
			_, err = client.ReplyToProjectMessage(context.Background(), "demo", parent, ProjectMessageInput{Body: &body}, actor)
			if err == nil || requests.Load() != 1 {
				t.Fatalf("err=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestAgentProjectMessageRejectsInvalidInputsBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := projectMessageActor()
	blank, alias := "", "would be ignored"
	for _, call := range []func() error{
		func() error {
			_, err := client.SendProjectMessage(context.Background(), " ", ProjectMessageInput{Content: &alias}, actor)
			return err
		},
		func() error {
			_, err := client.ReplyToProjectMessage(context.Background(), "demo", 0, ProjectMessageInput{Body: &alias}, actor)
			return err
		},
		func() error {
			_, err := client.TakeProjectMessageAction(context.Background(), "demo", 42, ProjectMessageInput{Body: &blank, Message: &alias}, actor)
			return err
		},
		func() error {
			_, err := client.SendProjectMessage(context.Background(), "demo", ProjectMessageInput{Body: &alias}, storage.AgentOrigin{})
			return err
		},
	} {
		if err := call(); err == nil {
			t.Fatal("accepted invalid local input")
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid input sent %d requests", requests.Load())
	}
}

func TestAgentProjectMessagesRoundTripThroughRealServer(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "agent-messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "native", ChannelName: "internal", RoomID: "native", Kind: "adhoc"}); err != nil {
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
	room, err := client.CreateProjectMessageRoom(ctx, "NATIVE")
	if err != nil || room == nil || room.Ident != "native" || room.ChannelName != "internal" || room.RoomID != "native" {
		t.Fatalf("room response=%+v err=%v", room, err)
	}
	actor := projectMessageActor()
	content := "  source body  "
	subject, hostname, eventAt := "Subject", "host", int64(123)
	root, err := client.SendProjectMessage(ctx, "native", ProjectMessageInput{Content: &content, Subject: &subject, Hostname: &hostname, EventAt: &eventAt}, actor)
	if err != nil || root.MessageID <= 0 || root.ExternalMessageID != fmt.Sprintf("internal:%d", root.MessageID) {
		t.Fatalf("root receipt=%+v err=%v", root, err)
	}
	replyBody := "reply"
	reply, err := client.ReplyToProjectMessage(ctx, "native", root.MessageID, ProjectMessageInput{Body: &replyBody}, actor)
	if err != nil || reply.ParentMessageID == nil || *reply.ParentMessageID != root.MessageID {
		t.Fatalf("reply receipt=%+v err=%v", reply, err)
	}
	actionBody := "action"
	blankSubject := ""
	action, err := client.TakeProjectMessageAction(ctx, "native", root.MessageID, ProjectMessageInput{Message: &actionBody, Subject: &blankSubject}, actor)
	if err != nil || action.ParentMessageID == nil || *action.ParentMessageID != root.MessageID {
		t.Fatalf("action receipt=%+v err=%v", action, err)
	}
	thread, err := client.GetProjectMessageThread(ctx, "native", root.MessageID)
	if err != nil || thread.Root.Content != content || thread.Root.AgentID == nil || *thread.Root.AgentID != actor.SessionID || thread.Root.Subject == nil || *thread.Root.Subject != subject || thread.Root.Hostname == nil || *thread.Root.Hostname != hostname || thread.Root.EventAt == nil || *thread.Root.EventAt != eventAt || len(thread.Replies) != 2 || thread.Replies[0].MessageType != "reply" || thread.Replies[1].MessageType != "action" || thread.Replies[1].Subject == nil || *thread.Replies[1].Subject != "action" {
		t.Fatalf("stored agent thread=%+v err=%v", thread, err)
	}
}

func TestExternalProjectRoomAndOpaqueMessageReceiptsRoundTripThroughServer(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "external-agent-messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "external", ChannelName: "discord", RoomID: "external-room", Kind: "repo"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandlerWithConfig(store, "message-key", server.HandlerConfig{Channels: map[string]channels.Outbound{"discord": sdkExternalMessageProvider{}}})
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	room, err := client.CreateProjectMessageRoom(ctx, "external")
	if err != nil || room == nil || room.ChannelName != "discord" || room.RoomID != "external-room" {
		t.Fatalf("external room=%+v err=%v", room, err)
	}
	origin := projectMessageActor()
	body := "external root"
	root, err := client.SendProjectMessage(ctx, "external", ProjectMessageInput{Body: &body}, origin)
	if err != nil || root.ExternalMessageID != "opaque-provider-root" {
		t.Fatalf("external root=%+v err=%v", root, err)
	}
	replyBody := "external reply"
	reply, err := client.ReplyToProjectMessage(ctx, "external", root.MessageID, ProjectMessageInput{Body: &replyBody}, origin)
	if err != nil || reply.ExternalMessageID != "opaque-provider-reply" || reply.ParentMessageID == nil || *reply.ParentMessageID != root.MessageID {
		t.Fatalf("external reply=%+v err=%v", reply, err)
	}
	storedRoot, err := store.ProjectMessage(ctx, "external", root.MessageID)
	if err != nil || storedRoot.ExternalMessageID == nil || *storedRoot.ExternalMessageID != "opaque-provider-root" {
		t.Fatalf("stored root=%+v err=%v", storedRoot, err)
	}
	storedReply, err := store.ProjectMessage(ctx, "external", reply.MessageID)
	if err != nil || storedReply.ExternalMessageID == nil || *storedReply.ExternalMessageID != "opaque-provider-reply" || storedReply.ParentMessageID == nil || *storedReply.ParentMessageID != root.MessageID {
		t.Fatalf("stored reply=%+v err=%v", storedReply, err)
	}
}

func TestCreateProjectMessageRoomRejectsWrongAcknowledgementWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/room" || r.Header.Get("X-Agent-Session-Id") != "" {
			t.Errorf("room request method=%s path=%s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		_, _ = fmt.Fprint(w, `{"ident":"other","channel_name":"internal","room_id":"other","kind":"adhoc"}`)
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateProjectMessageRoom(context.Background(), "demo"); err == nil || requests.Load() != 1 {
		t.Fatalf("err=%v requests=%d", err, requests.Load())
	}
}

func TestCreateProjectMessageRoomAcceptsConfiguredExternalProject(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/room" {
			t.Errorf("room request method=%s path=%s", r.Method, r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"ident":"demo","channel_name":"discord","room_id":"123","kind":"repo"}`)
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	project, err := client.CreateProjectMessageRoom(context.Background(), "DEMO")
	if err != nil || project == nil || project.Ident != "demo" || project.ChannelName != "discord" || project.RoomID != "123" || project.Kind != "repo" {
		t.Fatalf("external room project=%+v err=%v", project, err)
	}
}

func TestAgentProjectMessageCommittedLostAckIsNotRetried(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "agent-message-lost-ack.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "native", ChannelName: "internal", RoomID: "native", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	base, err := server.NewHandler(store, "message-key")
	if err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/projects/native/messages" {
			posts.Add(1)
			recorded := httptest.NewRecorder()
			base.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusOK {
				t.Errorf("agent message handler status=%d body=%s", recorded.Code, recorded.Body.String())
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("HTTP server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack acknowledgement: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := projectMessageActor()
	body := "persist exactly once"
	if receipt, err := client.SendProjectMessage(ctx, "native", ProjectMessageInput{Body: &body}, actor); err == nil || receipt != nil {
		t.Fatalf("lost acknowledgement receipt=%+v err=%v", receipt, err)
	}
	if posts.Load() != 1 {
		t.Fatalf("uncertain agent post replayed %d times", posts.Load())
	}
	list, err := store.ListProjectMessageThreads(ctx, "native", storage.ProjectMessageThreadFilter{State: "all", Limit: 100})
	if err != nil || len(list.Threads) != 1 || list.Threads[0].Preview != body {
		t.Fatalf("committed message list=%+v err=%v", list, err)
	}
}
