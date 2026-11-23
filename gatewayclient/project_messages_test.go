package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func projectMessageActor() storage.AgentOrigin {
	return storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
}

func TestUnreadProjectMessagesValidatesExactProjectAndNativeOrigin(t *testing.T) {
	actor := projectMessageActor()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/messages/unread" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Agent-Session-Id") != actor.SessionID || r.Header.Get("X-Agent-Instance-Id") != actor.InstanceID || r.Header.Get("X-Agent-Provider") != actor.Provider || r.Header.Get("X-Agent-OS") != actor.OS || r.Header.Get("X-Agent-Id") != "" {
			t.Errorf("native identity headers changed or alias was forwarded: %v", r.Header)
		}
		_, _ = fmt.Fprint(w, `{"status":"1 unread user message(s)","messages":[{"id":7,"project_ident":"demo","source":"user","content":"hello","sent_at":10,"message_type":"message","deliver_to_agents":false}]}`)
	}))
	defer host.Close()

	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.UnreadProjectMessages(context.Background(), "demo", actor)
	if err != nil || result.Status != "1 unread user message(s)" || len(result.Messages) != 1 || result.Messages[0].ID != 7 || result.Messages[0].ProjectIdent != "demo" {
		t.Fatalf("unread result=%+v err=%v", result, err)
	}
}

func TestUnreadProjectMessagesRejectsMismatchedProjectAndIDs(t *testing.T) {
	actor := projectMessageActor()
	for _, body := range []string{
		`{"status":"1 unread user message(s)","messages":[{"id":7,"project_ident":"other"}]}`,
		`{"status":"2 unread user message(s)","messages":[{"id":7,"project_ident":"demo"},{"id":7,"project_ident":"demo"}]}`,
		`{"status":"no messages","messages":null}`,
	} {
		t.Run(body, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer host.Close()
			client, err := New(host.URL, "message-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.UnreadProjectMessages(context.Background(), "demo", actor); err == nil {
				t.Fatalf("accepted mismatched unread response %s", body)
			}
		})
	}
}

func TestConfirmProjectMessageDoesNotRetryUncertainResponse(t *testing.T) {
	actor := projectMessageActor()
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/demo/messages/42/confirm" || r.ContentLength != 0 {
			t.Errorf("request = %s %s content-length=%d", r.Method, r.URL.Path, r.ContentLength)
		}
		if r.Header.Get("X-Agent-Session-Id") != actor.SessionID || r.Header.Get("X-Agent-Id") != "" {
			t.Errorf("confirmation identity headers=%v", r.Header)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ConfirmProjectMessage(context.Background(), "demo", 42, actor); err == nil || requests.Load() != 1 {
		t.Fatalf("uncertain confirmation err=%v requests=%d", err, requests.Load())
	}
}

func TestConfirmProjectMessageRequiresExactAcknowledgement(t *testing.T) {
	actor := projectMessageActor()
	for _, response := range []string{`{"confirmed":true}`, `{"confirmed":false}`, `{}`, `{"confirmed":"yes"}`} {
		t.Run(response, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, response) }))
			defer host.Close()
			client, err := New(host.URL, "message-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			confirmed, err := client.ConfirmProjectMessage(context.Background(), "demo", 42, actor)
			if response == `{"confirmed":true}` {
				if err != nil || !confirmed {
					t.Fatalf("confirmation=%t err=%v", confirmed, err)
				}
				return
			}
			if response == `{"confirmed":false}` {
				if err != nil || confirmed {
					t.Fatalf("repeat confirmation=%t err=%v", confirmed, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted malformed confirmation response %s", response)
			}
		})
	}
}

func TestProjectMessageDeliveryRoundTripUsesExactNativeSession(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, project := range []string{"source", "target"} {
		if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: "none", Kind: "adhoc"}); err != nil {
			t.Fatal(err)
		}
	}
	sourceActor := projectMessageActor()
	rootReason, details := "user-directed", "Delegation fixture"
	parent, err := store.InsertTask(ctx, storage.TaskInsert{Project: "source", Title: "Parent", Reporter: sourceActor.SessionID, RootReason: &rootReason, Details: &details}, &sourceActor)
	if err != nil {
		t.Fatal(err)
	}
	delegation, err := store.DelegateTask(ctx, storage.TaskDelegationInput{
		SourceProject: "source", TargetProject: "target", Title: "Native inbox task", Reporter: sourceActor.SessionID, ParentID: &parent.ID,
	}, &sourceActor)
	if err != nil {
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
	targetActor := projectMessageActor()
	otherTargetActor := projectMessageActor()
	unread, err := client.UnreadProjectMessages(ctx, "target", targetActor)
	if err != nil || len(unread.Messages) != 1 {
		t.Fatalf("target unread=%+v err=%v", unread, err)
	}
	nudge := unread.Messages[0]
	if nudge.ID != delegation.MessageID || nudge.Source != "system" || !nudge.DeliverToAgents || nudge.Subject == nil || *nudge.Subject != "Delegated task created" {
		t.Fatalf("delegation nudge not delivered from persisted message: %+v receipt=%d", nudge, delegation.MessageID)
	}
	otherInbox, err := client.UnreadProjectMessages(ctx, "target", otherTargetActor)
	if err != nil || len(otherInbox.Messages) != 1 || otherInbox.Messages[0].ID != nudge.ID {
		t.Fatalf("other session inbox=%+v err=%v", otherInbox, err)
	}
	if confirmed, err := client.ConfirmProjectMessage(ctx, "source", nudge.ID, targetActor); err == nil || confirmed {
		t.Fatalf("cross-project confirmation accepted=%t err=%v", confirmed, err)
	}
	if confirmed, err := client.ConfirmProjectMessage(ctx, "target", nudge.ID, targetActor); err != nil || !confirmed {
		t.Fatalf("confirm delegation nudge=%t err=%v", confirmed, err)
	}
	remaining, err := client.UnreadProjectMessages(ctx, "target", targetActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Messages) != 0 {
		t.Fatalf("confirmed session still sees nudge: %+v", remaining.Messages)
	}
	remaining, err = client.UnreadProjectMessages(ctx, "target", otherTargetActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Messages) != 1 || remaining.Messages[0].ID != nudge.ID {
		t.Fatalf("confirmation leaked across sessions: %+v err=%v", remaining, err)
	}
}

func TestConfirmProjectMessageCommittedLostAckIsNotRetried(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "lost-ack.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "inbox", ChannelName: "discord", RoomID: "room-a", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	event := router.Event{Seq: 1, ID: "channel.message_received", Version: 1, EmittedAt: 1234,
		Payload: []byte(`{"channel_name":"discord","room_id":"room-a","message":{"id":"msg-1","content":"persist this","author_kind":"human"}}`)}
	if stop, err := store.IngestRouterInbound(ctx, event); err != nil || stop {
		t.Fatalf("ingest message stop=%t err=%v", stop, err)
	}
	base, err := server.NewHandler(store, "message-key")
	if err != nil {
		t.Fatal(err)
	}
	var postCount atomic.Int32
	var getRemote, postRemote atomic.Value
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getRemote.Store(r.RemoteAddr)
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/projects/inbox/messages/1/confirm" {
			postCount.Add(1)
			postRemote.Store(r.RemoteAddr)
			recorded := httptest.NewRecorder()
			base.ServeHTTP(recorded, r) // commits the confirmation before dropping the reply
			if recorded.Code != http.StatusOK {
				t.Errorf("confirmation handler status=%d body=%s", recorded.Code, recorded.Body.String())
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("HTTP server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack response connection: %v", err)
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
	unread, err := client.UnreadProjectMessages(ctx, "inbox", actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread.Messages) != 1 {
		t.Fatalf("initial unread=%+v", unread)
	}
	if confirmed, err := client.ConfirmProjectMessage(ctx, "inbox", 1, actor); err == nil || confirmed {
		t.Fatalf("lost confirmation acknowledgement confirmed=%t err=%v", confirmed, err)
	}
	if postCount.Load() != 1 {
		t.Fatalf("uncertain confirmation was replayed %d times", postCount.Load())
	}
	if getRemote.Load() != postRemote.Load() {
		t.Fatalf("confirmation did not reuse the inspected connection: GET=%v POST=%v", getRemote.Load(), postRemote.Load())
	}
	unread, err = client.UnreadProjectMessages(ctx, "inbox", actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread.Messages) != 0 {
		t.Fatalf("committed confirmation still unread=%+v", unread)
	}
	otherActor := projectMessageActor()
	unread, err = client.UnreadProjectMessages(ctx, "inbox", otherActor)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread.Messages) != 1 {
		t.Fatalf("other session inbox=%+v", unread)
	}
}
