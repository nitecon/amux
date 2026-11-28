package amuxruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestFleetRetryBackoff(t *testing.T) {
	var retry fleetRetry
	now := time.Now()
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		retry.failed(now)
		if retry.delay != want || !retry.next.Equal(now.Add(want)) {
			t.Fatalf("retry = %+v, want delay %s", retry, want)
		}
		now = retry.next
	}
}

func TestFleetMissingRolloutDoesNotRestartOnReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openStore := func() *storage.Store {
		s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	remote, local := openStore(), openStore()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	snapshots := make(chan []storage.FleetConversation)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/fleet/conversations/stream" {
			handler.ServeHTTP(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		stream := conn.CloseRead(r.Context())
		for {
			select {
			case <-stream.Done():
				return
			case chats := <-snapshots:
				if err := wsjson.Write(stream, conn, map[string]any{"version": 1, "chats": chats}); err != nil {
					return
				}
			}
		}
	}))
	defer host.Close()
	client, err := gatewayclient.New(host.URL, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	pool, err := local.CreateWorkerPool(ctx, storage.WorkerPool{ID: uuid.NewString(), Server: client.Endpoint(), Project: "demo", Directory: directory, Provider: "codex", OS: platform, SlotCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := slots[0].Owner
	chat, err := remote.CreateConversation(ctx, "demo", "", &owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveWorkerRun(ctx, storage.WorkerRun{ID: chat.ID, ConversationID: chat.ID, Project: "demo", Server: client.Endpoint(), Directory: directory, Owner: owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.QueueWorkerEvent(ctx, chat.ID, conversation.Event{Kind: "session_ready", SessionID: "missing-rollout"}); err != nil {
		t.Fatal(err)
	}
	starts := make(chan string, 10)
	open := func(_ context.Context, run storage.WorkerRun, _ conversation.Sink) (providers.Session, error) {
		starts <- run.NativeSessionID
		return nil, errors.New("provider RPC error -32600: no rollout found for thread id missing-rollout")
	}
	changed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFleetConversations(ctx, local, client, open, nil, nil, func(string, string) string { return directory }, changed, io.Discard)
	}()
	defer func() { cancel(); <-done }()
	select {
	case snapshots <- []storage.FleetConversation{{Conversation: *chat}}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case native := <-starts:
		if native != "missing-rollout" {
			t.Fatalf("resume lost native session: %q", native)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Wait for the exit handler to publish the user-visible retry explanation.
	for {
		events, err := remote.ConversationEvents(ctx, "demo", chat.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range events {
			found = found || event.Event.Kind == "local_error" && strings.Contains(event.Event.Text, "no rollout found") && strings.Contains(event.Event.Text, "fleet close")
		}
		if found {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("missing user-visible failure")
		case <-time.After(5 * time.Millisecond):
		}
	}
	for range 20 {
		select {
		case changed <- struct{}{}:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Include a repeated desired-state snapshot, as on stream reconnect.
	select {
	case snapshots <- []storage.FleetConversation{{Conversation: *chat}}:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case native := <-starts:
		t.Fatalf("failed rollout restarted during backoff: %q", native)
	case <-time.After(100 * time.Millisecond):
	}
}
