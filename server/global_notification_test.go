package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestGlobalNotificationPublishesAndReplaysIndependently(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "global-notifications.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	builtins, err := NewGatewayFISM(store, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithRemote(store, "stream-secret", RemoteConfig{Context: ctx, Registry: builtins.Registry(), Forwarded: controllers.RemoteForwardedEvents()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx)

	publish := func(body string, authorized bool, want int) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/notifications/global", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if authorized {
			req.Header.Set("Authorization", "Bearer stream-secret")
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("publish status %d, want %d", res.StatusCode, want)
		}
	}
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/events/stream"
	subscribe := func(id string, after bool) *websocket.Conn {
		t.Helper()
		conn := connectStream(t, url)
		sub := map[string]any{"type": "subscribe", "consumer_id": id, "events": []string{controllers.GlobalNotifyEventID}}
		if after {
			sub["after"] = 0
		}
		sendStream(t, conn, sub)
		if frame := readStream(t, conn); frameType(frame) != "subscribed" {
			t.Fatalf("subscribe: %s", frame)
		}
		return conn
	}
	firstID, secondID := uuid.NewString(), uuid.NewString()
	first, second := subscribe(firstID, true), subscribe(secondID, true)
	defer first.CloseNow()
	defer second.CloseNow()
	sendStream(t, second, map[string]any{"type": "emit", "id": controllers.GlobalNotifyEventID, "version": 1, "payload": map[string]string{"message": "bypass"}})
	if frame := readStream(t, second); frameType(frame) != "error" {
		t.Fatalf("WebSocket accepted direct broadcast: %s", frame)
	}
	sharedKey := connectStream(t, url)
	defer sharedKey.CloseNow()
	sendStream(t, sharedKey, map[string]any{"type": "subscribe", "consumer_id": uuid.NewString(), "events": []string{controllers.NotifyEventID}})
	if frame := readStream(t, sharedKey); frameType(frame) != "error" {
		t.Fatalf("shared key accepted private subscription: %s", frame)
	}
	publish(`{"message":"maintenance"}`, false, http.StatusUnauthorized)
	publish(`{"message":"maintenance","target":"private"}`, true, http.StatusBadRequest)
	publish(`{"message":"maintenance"}`, true, http.StatusCreated)
	firstFrame, secondFrame := readStream(t, first), readStream(t, second)
	if frameType(firstFrame) != "event" || frameType(secondFrame) != "event" ||
		eventUID(t, firstFrame) != eventUID(t, secondFrame) || !strings.Contains(string(firstFrame["event"]), "maintenance") {
		t.Fatalf("expected same broadcast on both sockets: %s, %s", firstFrame, secondFrame)
	}
	var delivered fism.Event
	if err := json.Unmarshal(secondFrame["event"], &delivered); err != nil {
		t.Fatal(err)
	}
	if deliveries, err := store.RouterDeliveries(ctx, delivered.Seq); err != nil || len(deliveries) != 0 {
		t.Fatalf("global broadcast admitted controller work: %+v %v", deliveries, err)
	}
	sendStream(t, second, map[string]any{"type": "ack", "seq": delivered.Seq})
	if frame := readStream(t, second); frameType(frame) != "acked" {
		t.Fatalf("second consumer stalled: %s", frame)
	}
	reconnected := subscribe(firstID, false)
	defer reconnected.CloseNow()
	if frame := readStream(t, reconnected); frameType(frame) != "event" || eventUID(t, frame) != delivered.UID {
		t.Fatalf("first consumer's pending broadcast did not replay: %s", frame)
	}
}

func TestAgentNotificationRequiresBoundEnrolledRecipient(t *testing.T) {
	ctx := context.Background()
	store, enrollment, _, first, _, srv, firstClient, secondClient := secureFixture(t, true)
	actor := agent.Origin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	const project = "agent-notifications"
	if err := store.InsertProject(ctx, storage.Project{Ident: project, ChannelName: project, RoomID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := enrollment.BindInstance(ctx, first.response.NodeID, actor.InstanceID); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.ReportPresenceWithPrincipal(ctx, storage.PresenceReport{Source: "native:recipient", Sequence: 1, Agents: []storage.PresenceAgent{{ProjectIdent: project, Origin: actor}}}, time.Now(), first.response.NodeID)
	if err != nil || !accepted {
		t.Fatalf("register recipient: %t %v", accepted, err)
	}
	forwarded := []string{controllers.NotifyEventID, controllers.GlobalNotifyEventID}
	registry, err := fism.NewRegistry(controllers.NotificationEventDefinitions(), []fism.ControllerDefinition{controllers.RemoteController(forwarded).Definition})
	if err != nil {
		t.Fatal(err)
	}
	emit := func(id string, payload any) fism.Event {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: id, Version: 1, Source: "fixture", Payload: body})
		if err != nil || !result.Stored {
			t.Fatalf("emit %s: %+v %v", id, result, err)
		}
		return result.Event
	}
	notice := emit(controllers.NotifyEventID, map[string]any{
		"target": actor.SessionID, "project": project, "mode": "recipient", "require_idle": false,
		"receiver": map[string]string{"session_id": actor.SessionID, "instance_id": actor.InstanceID, "project": project},
		"event":    map[string]any{"kind": "task_commented", "task": map[string]string{"id": "private-task"}},
	})
	broadcast := emit(controllers.GlobalNotifyEventID, map[string]string{"message": "visible to both"})
	url := "wss" + strings.TrimPrefix(srv.URL, "https") + "/v1/events/stream"
	subscribe := func(client *http.Client, id string, replay bool) *websocket.Conn {
		t.Helper()
		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		request := map[string]any{"type": "subscribe", "consumer_id": id, "events": forwarded, "targets": []string{actor.SessionID}}
		if replay {
			request["after"] = 0
		}
		sendStream(t, conn, request)
		if frame := readStream(t, conn); frameType(frame) != "subscribed" {
			t.Fatalf("subscribe: %s", frame)
		}
		return conn
	}
	firstID := uuid.NewString()
	intended := subscribe(firstClient, firstID, true)
	other := subscribe(secondClient, uuid.NewString(), true)
	if frame := readStream(t, intended); frameType(frame) != "event" || eventUID(t, frame) != notice.UID {
		t.Fatalf("intended recipient did not receive notice: %s", frame)
	}
	if frame := readStream(t, other); frameType(frame) != "event" || eventUID(t, frame) != broadcast.UID {
		t.Fatalf("unrelated node did not skip private notice: %s", frame)
	}
	replaced := subscribe(firstClient, firstID, false)
	if frame := readStream(t, replaced); frameType(frame) != "event" || eventUID(t, frame) != notice.UID {
		t.Fatalf("private notice did not replay: %s", frame)
	}
}

func TestGlobalNotificationClientReplaysUntilHandled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "desktop-notifications.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	builtins, err := NewGatewayFISM(store, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithRemote(store, "stream-secret", RemoteConfig{Context: ctx, Registry: builtins.Registry(), Forwarded: controllers.RemoteForwardedEvents()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx)
	client, err := gatewayclient.New(srv.URL, "stream-secret", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	consumer, connection := uuid.NewString(), uuid.NewString()
	after := int64(0)
	if _, err := store.OpenRemoteConsumer(ctx, consumer, connection, &after); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"message":"first"}`, `{"message":"next"}`} {
		result, err := store.EmitRouterEvent(ctx, builtins.Registry(), fism.EmitRequest{
			ID: controllers.GlobalNotifyEventID, Version: 1, Source: "fixture", Payload: []byte(body),
		})
		if err != nil || !result.Stored {
			t.Fatalf("emit notice: %+v %v", result, err)
		}
	}

	unhandled := errors.New("display unavailable")
	if err := client.WatchGlobalNotifications(ctx, consumer, func(_, message string) error {
		if message != "first" {
			t.Fatalf("first notification = %q", message)
		}
		return unhandled
	}); !errors.Is(err, unhandled) {
		t.Fatalf("watch returned %v", err)
	}
	var handled []string
	if err := client.WatchGlobalNotifications(ctx, consumer, func(_, message string) error {
		handled = append(handled, message)
		if message == "next" {
			return unhandled
		}
		return nil
	}); !errors.Is(err, unhandled) || !slices.Equal(handled, []string{"first", "next"}) {
		t.Fatalf("reconnect handled %v and returned %v", handled, err)
	}
}
