package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestPostHumanProjectMessagePinsThreadRootAndOmitsNativeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name              string
		parent            int64
		thread            string
		wantReceiptParent int64
		wantParentSet     bool
	}{
		{
			name: "reply points at normalized root", parent: 8,
			thread:            `{"root":{"id":5,"project_ident":"demo","message_type":"message"},"state":"open","replies":[{"id":8,"project_ident":"demo","parent_message_id":5}],"confirmations":[]}`,
			wantReceiptParent: 5, wantParentSet: true,
		},
		{
			name: "root reply uses itself as parent", parent: 8,
			thread:            `{"root":{"id":8,"project_ident":"demo","message_type":"message"},"state":"open","replies":[],"confirmations":[]}`,
			wantReceiptParent: 8, wantParentSet: true,
		},
		{
			name: "legacy self-parent uses itself as parent", parent: 8,
			thread:            `{"root":{"id":8,"project_ident":"demo","parent_message_id":8,"message_type":"reply"},"state":"open","replies":[],"confirmations":[]}`,
			wantReceiptParent: 8, wantParentSet: true,
		},
		{
			name: "nested reply preserves source one-hop parent", parent: 8,
			thread:            `{"root":{"id":5,"project_ident":"demo","parent_message_id":3,"message_type":"reply"},"state":"answered","replies":[{"id":8,"project_ident":"demo","parent_message_id":5}],"confirmations":[]}`,
			wantReceiptParent: 5, wantParentSet: true,
		},
		{
			name: "orphan reply follows source fallback to new root", parent: 8,
			thread: `{"root":{"id":8,"project_ident":"demo","parent_message_id":3,"message_type":"reply"},"state":"open","replies":[],"confirmations":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var getCount, postCount atomic.Int32
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Agent-Session-Id") != "" || r.Header.Get("X-Agent-Id") != "" {
					t.Errorf("human message request sent native identity: %v", r.Header)
				}
				switch r.Method {
				case http.MethodGet:
					getCount.Add(1)
					if r.URL.Path != fmt.Sprintf("/v1/projects/demo/messages/%d/thread", tc.parent) {
						t.Errorf("thread request path=%s", r.URL.Path)
					}
					_, _ = fmt.Fprint(w, tc.thread)
				case http.MethodPost:
					postCount.Add(1)
					if r.URL.Path != "/v1/projects/demo/messages/human" {
						t.Errorf("post request path=%s", r.URL.Path)
					}
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode post body: %v", err)
					}
					if string(body["content"]) != `"reply body"` || string(body["author"]) != `"Display only"` {
						t.Errorf("request body=%s", mustJSON(t, body))
					}
					if string(body["parent_message_id"]) != fmt.Sprint(tc.parent) {
						t.Errorf("POST did not preserve requested parent: got=%s want=%d", body["parent_message_id"], tc.parent)
					}
					parent := "null"
					if tc.wantParentSet {
						parent = fmt.Sprint(tc.wantReceiptParent)
					}
					_, _ = fmt.Fprintf(w, `{"message_id":9,"parent_message_id":%s,"delivered":false,"delivery_error":"channel plugin 'discord' is not configured"}`, parent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer host.Close()
			client, err := New(host.URL, "message-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := client.PostHumanProjectMessage(context.Background(), "demo", HumanProjectMessageInput{
				Content: " reply body ", ParentMessageID: &tc.parent, Author: " Display only ",
			})
			if err != nil || receipt.MessageID != 9 || receipt.Delivered || receipt.DeliveryError == nil || getCount.Load() != 1 || postCount.Load() != 1 {
				t.Fatalf("receipt=%+v err=%v GET=%d POST=%d", receipt, err, getCount.Load(), postCount.Load())
			}
			if (receipt.ParentMessageID != nil) != tc.wantParentSet || tc.wantParentSet && *receipt.ParentMessageID != tc.wantReceiptParent {
				t.Fatalf("acknowledged parent=%v", receipt.ParentMessageID)
			}
		})
	}
}

func TestPostHumanProjectMessageDoesNotWriteWhenThreadPreflightIsInvalid(t *testing.T) {
	var posts atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			return
		}
		_, _ = fmt.Fprint(w, `{"root":{"id":5,"project_ident":"other"},"state":"open","replies":[{"id":8,"project_ident":"other","parent_message_id":5}],"confirmations":[]}`)
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := int64(8)
	if _, err := client.PostHumanProjectMessage(context.Background(), "demo", HumanProjectMessageInput{Content: "reply", ParentMessageID: &parent}); err == nil {
		t.Fatal("accepted a thread from another project")
	}
	if posts.Load() != 0 {
		t.Fatalf("posted despite invalid thread preflight: %d", posts.Load())
	}
}

func TestParseHumanProjectMessageReceiptRequiresExactFieldsAndParentOrder(t *testing.T) {
	parent := int64(5)
	for _, body := range []string{
		`{"message_id":5,"parent_message_id":5,"delivered":true,"delivery_error":null}`,
		`{"message_id":4,"parent_message_id":5,"delivered":true,"delivery_error":null}`,
		`{"message_id":6,"parent_message_id":null,"delivered":true,"delivery_error":null}`,
		`{"message_id":6,"parent_message_id":5,"delivered":false,"delivery_error":null}`,
		`{"message_id":6,"parent_message_id":5,"delivered":true,"delivery_error":"failed"}`,
		`{"message_id":6,"parent_message_id":5,"delivered":true}`,
		`{"message_id":null,"parent_message_id":5,"delivered":true,"delivery_error":null}`,
	} {
		if _, err := parseHumanProjectMessageReceipt(json.RawMessage(body), &parent); err == nil {
			t.Errorf("accepted malformed receipt %s", body)
		}
	}
	for _, body := range []string{
		`{"message_id":6,"parent_message_id":null,"delivered":true,"delivery_error":null}`,
		`{"message_id":6,"parent_message_id":5,"delivered":false,"delivery_error":"delivery failed"}`,
	} {
		var expected *int64
		if strings.Contains(body, `"parent_message_id":5`) {
			expected = &parent
		}
		if _, err := parseHumanProjectMessageReceipt(json.RawMessage(body), expected); err != nil {
			t.Errorf("rejected valid receipt %s: %v", body, err)
		}
	}
}

func TestPostHumanProjectMessageRoundTripDurableAndConfirmedPerNativeSession(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "human-message.db"))
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
	actorA, actorB := projectMessageActor(), projectMessageActor()
	receipt, err := client.PostHumanProjectMessage(ctx, "native", HumanProjectMessageInput{Content: "  hello agents  "})
	if err != nil || receipt.MessageID <= 0 || receipt.ParentMessageID != nil || !receipt.Delivered || receipt.DeliveryError != nil {
		t.Fatalf("native post receipt=%+v err=%v", receipt, err)
	}
	for _, actor := range []agent.Origin{actorA, actorB} {
		unread, err := client.UnreadProjectMessages(ctx, "native", actor)
		if err != nil || len(unread.Messages) != 1 || unread.Messages[0].ID != receipt.MessageID {
			t.Fatalf("native session %s unread=%+v err=%v", actor.SessionID, unread, err)
		}
	}
	if confirmed, err := client.ConfirmProjectMessage(ctx, "native", receipt.MessageID, actorA); err != nil || !confirmed {
		t.Fatalf("confirm human message=%t err=%v", confirmed, err)
	}
	for _, tc := range []struct {
		actor agent.Origin
		count int
	}{{actorA, 0}, {actorB, 1}} {
		unread, err := client.UnreadProjectMessages(ctx, "native", tc.actor)
		if err != nil || len(unread.Messages) != tc.count {
			t.Fatalf("session %s unread after A ack=%+v err=%v wantCount=%d", tc.actor.SessionID, unread, err, tc.count)
		}
	}
	thread, err := client.GetProjectMessageThread(ctx, "native", receipt.MessageID)
	if err != nil || thread.Root.ID != receipt.MessageID || thread.Root.ProjectIdent != "native" || thread.Root.Content != "hello agents" || thread.Root.Source != "user" || thread.Root.AuthorKind == nil || *thread.Root.AuthorKind != "human" || thread.Root.AgentID != nil || !thread.Root.DeliverToAgents || len(thread.Confirmations) != 1 || thread.Confirmations[0].AgentID != actorA.SessionID {
		t.Fatalf("durable human message thread=%+v err=%v", thread, err)
	}
}

func TestPostHumanProjectMessageCommittedLostAckIsNotRetried(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "lost-human-ack.db"))
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
		if r.Method == http.MethodPost && r.URL.Path == "/v1/projects/native/messages/human" {
			posts.Add(1)
			recorded := httptest.NewRecorder()
			base.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusOK {
				t.Errorf("human message handler status=%d body=%s", recorded.Code, recorded.Body.String())
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
	if _, err := client.UnreadProjectMessages(ctx, "native", actor); err != nil {
		t.Fatal(err)
	}
	if receipt, err := client.PostHumanProjectMessage(ctx, "native", HumanProjectMessageInput{Content: "persist exactly once"}); err == nil || receipt != nil {
		t.Fatalf("lost acknowledgement receipt=%+v err=%v", receipt, err)
	}
	if posts.Load() != 1 {
		t.Fatalf("uncertain human post replayed %d times", posts.Load())
	}
	unread, err := client.UnreadProjectMessages(ctx, "native", actor)
	if err != nil || len(unread.Messages) != 1 || unread.Messages[0].Content != "persist exactly once" {
		t.Fatalf("committed human message unread=%+v err=%v", unread, err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
