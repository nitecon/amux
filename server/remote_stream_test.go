package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/storage"
)

func remoteStreamFixture(t *testing.T) (*storage.Store, http.Handler, string, *fism.Registry) {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := fism.NewRegistry([]fism.EventDefinition{
		{ID: "remote.accepted", Version: 1, RemoteEmit: true, Validate: fism.Object},
		{ID: "local.only", Version: 1, Validate: fism.Object},
	}, []fism.ControllerDefinition{{ID: "stream-test", Subscriptions: []fism.Subscription{
		{Event: "remote.accepted", MinVersion: 1, MaxVersion: 1},
		{Event: "local.only", MinVersion: 1, MaxVersion: 1},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithRemote(store, "stream-secret", RemoteConfig{Context: ctx, Registry: registry, Forwarded: []string{"remote.accepted", "local.only"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(shutdown)
		srv.Close()
		_ = store.Close()
	})
	return store, handler, "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/events/stream", registry
}

func TestRemoteStreamPassiveHandlerAndAuth(t *testing.T) {
	store, _, _, registry := remoteStreamFixture(t)
	// The constructor's readiness is deliberately explicit: NewHandler stays passive.
	passive, err := NewHandler(store, "stream-secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(passive)
	defer server.Close()
	response, err := http.Get(server.URL + "/v1/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated passive endpoint status %d", response.StatusCode)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/events/stream", nil)
	request.Header.Set("Authorization", "Bearer stream-secret")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("passive endpoint status %d, want 503", response.StatusCode)
	}
	// The attached fixture has a live router; an expired context is rejected at construction.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewHandlerWithRemote(store, "stream-secret", RemoteConfig{Context: ctx, Registry: registry}); err == nil {
		t.Fatal("accepted stopped remote runtime")
	}
}

func TestRemoteStreamEmitStrictFrameAndSubscriptionAck(t *testing.T) {
	store, _, url, _ := remoteStreamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization":             []string{"Bearer stream-secret"},
		"X-Authenticated-Principal": []string{"forged-principal"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	for _, raw := range []string{
		`{"type":"heartbeat","source":"forged"}`,
		`{"type":"heartbeat","principal":"api-key"}`,
	} {
		if err := conn.Write(context.Background(), websocket.MessageText, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if got := frameType(readStream(t, conn)); got != "error" {
			t.Fatalf("accepted identity-bearing frame: %s", got)
		}
	}
	sendStream(t, conn, map[string]any{"type": "emit", "id": "remote.accepted", "version": 1, "payload": map[string]any{"source": "opaque payload"}})
	if got := readStream(t, conn); frameType(got) != "emitted" {
		t.Fatalf("accepted emit response: %s", got)
	}
	sendStream(t, conn, map[string]any{"type": "emit", "id": "local.only", "version": 1, "payload": map[string]any{}})
	if got := readStream(t, conn); frameType(got) != "error" {
		t.Fatalf("non-remote event accepted: %s", got)
	}
	events, err := store.RouterEvents(context.Background(), 0, 10)
	if err != nil || len(events) != 1 || events[0].Source != "client:api-key" {
		t.Fatalf("stored event source/events: %+v %v", events, err)
	}
	consumer := uuid.NewString()
	sendStream(t, conn, map[string]any{"type": "subscribe", "consumer_id": strings.ToUpper(consumer), "events": []string{"remote.accepted"}, "after": 0})
	if got := readStream(t, conn); frameType(got) != "subscribed" {
		t.Fatalf("subscription response: %s", got)
	}
	frame := readStream(t, conn)
	if frameType(frame) != "event" {
		t.Fatalf("expected replay event: %s", frame)
	}
	var event struct {
		Event struct {
			Seq int64 `json:"seq"`
		} `json:"event"`
	}
	if err := json.Unmarshal(mustMarshal(t, frame), &event); err != nil {
		t.Fatal(err)
	}
	sendStream(t, conn, map[string]any{"type": "ack", "seq": event.Event.Seq})
	if got := readStream(t, conn); frameType(got) != "acked" {
		t.Fatalf("ack response: %s", got)
	}
	// Duplicate acknowledgements at or behind the durable cursor are idempotent.
	sendStream(t, conn, map[string]any{"type": "ack", "seq": event.Event.Seq})
	if got := readStream(t, conn); frameType(got) != "acked" {
		t.Fatalf("duplicate ack response: %s", got)
	}
}

func TestRemoteStreamTakeoverAndShutdown(t *testing.T) {
	_, handler, url, _ := remoteStreamFixture(t)
	consumer := uuid.NewString()
	first := connectStream(t, url)
	sendStream(t, first, map[string]any{"type": "subscribe", "consumer_id": consumer, "events": []string{}})
	if got := readStream(t, first); frameType(got) != "subscribed" {
		t.Fatalf("first subscribe: %s", got)
	}
	second := connectStream(t, url)
	sendStream(t, second, map[string]any{"type": "subscribe", "consumer_id": consumer, "events": []string{}})
	if got := readStream(t, second); frameType(got) != "subscribed" {
		t.Fatalf("replacement subscribe: %s", got)
	}
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := first.Read(readCtx); err == nil || readCtx.Err() != nil {
		t.Fatalf("replaced consumer connection remained active: %v", err)
	}
	sendStream(t, second, map[string]any{"type": "heartbeat"})
	if got := readStream(t, second); frameType(got) != "heartbeat_ack" {
		t.Fatalf("replacement lost its lease: %s", got)
	}
	shutdown, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(shutdown); err != nil {
		t.Fatalf("stream shutdown did not join sockets: %v", err)
	}
}

func TestRemoteStreamRetryAckReplayWithoutTick(t *testing.T) {
	store, _, url, registry := remoteStreamFixture(t)
	ctx := context.Background()
	first, err := store.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "remote.accepted", Version: 1, Source: "plugin:test", Payload: json.RawMessage(`{"n":1}`)})
	if err != nil || !first.Stored {
		t.Fatalf("store first event: %+v %v", first, err)
	}
	second, err := store.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "remote.accepted", Version: 1, Source: "plugin:test", Payload: json.RawMessage(`{"n":2}`)})
	if err != nil || !second.Stored {
		t.Fatalf("store second event: %+v %v", second, err)
	}
	consumer := uuid.NewString()
	conn := connectStream(t, url)
	sendStream(t, conn, map[string]any{"type": "subscribe", "consumer_id": consumer, "events": []string{"remote.accepted"}, "after": 0})
	if got := readStream(t, conn); frameType(got) != "subscribed" {
		t.Fatalf("subscribe: %s", got)
	}
	firstFrame := readStream(t, conn)
	if frameType(firstFrame) != "event" || eventUID(t, firstFrame) != first.Event.UID {
		t.Fatalf("first event: %s", firstFrame)
	}
	// No shared Tick is published here. The owned deadline must resend after
	// the router's one-second retry delay using the identical event body.
	ctxRead, cancelRead := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	_, resend, err := conn.Read(ctxRead)
	cancelRead()
	if err != nil {
		t.Fatalf("deadline retry was not sent: %v", err)
	}
	var resendFrame map[string]json.RawMessage
	if err := json.Unmarshal(resend, &resendFrame); err != nil || frameType(resendFrame) != "event" || eventUID(t, resendFrame) != first.Event.UID {
		t.Fatalf("retry changed event: %s %v", resend, err)
	}
	sendStream(t, conn, map[string]any{"type": "ack", "seq": second.Event.Seq})
	if got := readStream(t, conn); frameType(got) != "error" {
		t.Fatalf("future ack accepted: %s", got)
	}
	sendStream(t, conn, map[string]any{"type": "ack", "seq": first.Event.Seq})
	if got := readStream(t, conn); frameType(got) != "acked" {
		t.Fatalf("first ack: %s", got)
	}
	secondFrame := readStream(t, conn)
	if frameType(secondFrame) != "event" || eventUID(t, secondFrame) != second.Event.UID {
		t.Fatalf("second event: %s", secondFrame)
	}
	conn.CloseNow() // Leave the second delivery unacknowledged.

	reconnected := connectStream(t, url)
	sendStream(t, reconnected, map[string]any{"type": "subscribe", "consumer_id": consumer, "events": []string{"remote.accepted"}})
	if got := readStream(t, reconnected); frameType(got) != "subscribed" {
		t.Fatalf("reconnect subscribe: %s", got)
	}
	replay := readStream(t, reconnected)
	if frameType(replay) != "event" || eventUID(t, replay) != second.Event.UID {
		t.Fatalf("unacked event not replayed: %s", replay)
	}
	sendStream(t, reconnected, map[string]any{"type": "ack", "seq": second.Event.Seq})
	if got := readStream(t, reconnected); frameType(got) != "acked" {
		t.Fatalf("replay ack: %s", got)
	}
}

func TestRemoteStreamDeliversAndAcksLargeEventPayload(t *testing.T) {
	store, _, url, registry := remoteStreamFixture(t)
	ctx := context.Background()
	largePayload := json.RawMessage(`{"body":"` + strings.Repeat("x", 128<<10) + `"}`)
	first, err := store.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "remote.accepted", Version: 1, Source: "plugin:test", Payload: largePayload})
	if err != nil || !first.Stored {
		t.Fatalf("store large event: %+v %v", first, err)
	}
	second, err := store.EmitRouterEvent(ctx, registry, fism.EmitRequest{ID: "remote.accepted", Version: 1, Source: "plugin:test", Payload: json.RawMessage(`{"body":"next"}`)})
	if err != nil || !second.Stored {
		t.Fatalf("store next event: %+v %v", second, err)
	}
	conn := connectStream(t, url)
	conn.SetReadLimit(2 << 20)
	sendStream(t, conn, map[string]any{"type": "subscribe", "consumer_id": uuid.NewString(), "events": []string{"remote.accepted"}, "after": 0})
	if got := readStream(t, conn); frameType(got) != "subscribed" {
		t.Fatalf("subscribe: %s", got)
	}
	frame := readStream(t, conn)
	if frameType(frame) != "event" {
		t.Fatalf("large event frame: %s", frame)
	}
	var delivered struct {
		Event struct {
			Seq     int64           `json:"seq"`
			UID     string          `json:"uid"`
			Payload json.RawMessage `json:"payload"`
		} `json:"event"`
	}
	if err := json.Unmarshal(mustMarshal(t, frame), &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.Event.UID != first.Event.UID || delivered.Event.Seq != first.Event.Seq || string(delivered.Event.Payload) != string(largePayload) {
		t.Fatalf("large payload was altered: uid=%q seq=%d payload bytes=%d want %d", delivered.Event.UID, delivered.Event.Seq, len(delivered.Event.Payload), len(largePayload))
	}
	sendStream(t, conn, map[string]any{"type": "ack", "seq": delivered.Event.Seq})
	if got := readStream(t, conn); frameType(got) != "acked" {
		t.Fatalf("large event ack: %s", got)
	}
	next := readStream(t, conn)
	if frameType(next) != "event" || eventUID(t, next) != second.Event.UID {
		t.Fatalf("next event not delivered after large ack: %s", next)
	}
}

func TestRemoteStreamBinaryAndOversizeFrames(t *testing.T) {
	_, _, url, _ := remoteStreamFixture(t)
	conn := connectStream(t, url)
	if err := conn.Write(context.Background(), websocket.MessageBinary, []byte(`{"type":"heartbeat"}`)); err != nil {
		t.Fatal(err)
	}
	sendStream(t, conn, map[string]any{"type": "heartbeat"})
	if got := readStream(t, conn); frameType(got) != "heartbeat_ack" {
		t.Fatalf("binary frame disturbed text protocol: %s", got)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(strings.Repeat("x", streamFrameBytes+1))); err != nil {
		t.Fatalf("send oversized frame: %v", err)
	}
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, readErr := conn.Read(readCtx)
	if readErr == nil {
		t.Fatal("oversized frame did not close the connection")
	}
}

func TestRemoteRuntimeDetachClosesAttachedStreams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "remote-detach.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := fism.NewRegistry(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandlerWithRemote(store, "stream-secret", RemoteConfig{Context: ctx, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	conn := connectStream(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/events/stream")
	cancel()
	readCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Fatal("runtime detach left stream open")
	}
	shutdown, stopShutdown := context.WithTimeout(context.Background(), time.Second)
	defer stopShutdown()
	if err := handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(shutdown); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func eventUID(t *testing.T, frame map[string]json.RawMessage) string {
	t.Helper()
	var event struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal(frame["event"], &event); err != nil {
		t.Fatal(err)
	}
	return event.UID
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
