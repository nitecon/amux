package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/storage"
)

func streamFixture(t *testing.T) (*storage.Store, string) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "stream-secret")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		if closer, ok := handler.(interface{ ShutdownStreams(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			closer.ShutdownStreams(ctx)
		}
		srv.Close()
		store.Close()
	})
	return store, "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/tasks/stream"
}
func connectStream(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer stream-secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(65536)
	t.Cleanup(func() { c.CloseNow() })
	return c
}
func sendStream(t *testing.T, c *websocket.Conn, value any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, value); err != nil {
		t.Fatal(err)
	}
}
func readStream(t *testing.T, c *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var value map[string]json.RawMessage
	if err := wsjson.Read(ctx, c, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func frameType(frame map[string]json.RawMessage) string {
	var typ string
	json.Unmarshal(frame["type"], &typ)
	return typ
}

func TestStreamPushReplayAndReceipts(t *testing.T) {
	store, url := streamFixture(t)
	c := connectStream(t, url)
	sendStream(t, c, map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "desktop", "after_event_id": 0})
	if frame := readStream(t, c); frameType(frame) != "subscribed" {
		t.Fatalf("subscribe %s", frame)
	}
	id, err := store.AppendTaskEvent(context.Background(), storage.TaskEvent{Kind: "created", ProjectIdent: "example", Task: json.RawMessage(`{"id":"task","description":"stream-secret message"}`)})
	if err != nil {
		t.Fatal(err)
	}
	frame := readStream(t, c)
	if frameType(frame) != "event" || strings.Contains(string(frame["event"]), "stream-secret") {
		t.Fatalf("event %s", frame)
	}
	var event storage.TaskEvent
	if err := json.Unmarshal(frame["event"], &event); err != nil {
		t.Fatal(err)
	}
	if event.ID != id || len(event.Deliveries) != 0 {
		t.Fatalf("event %+v", event)
	}
	sendStream(t, c, map[string]any{"type": "ack", "event_id": id + 1, "status": "received"})
	if frame := readStream(t, c); frameType(frame) != "error" {
		t.Fatalf("out-of-flight ack accepted %s", frame)
	}
	sendStream(t, c, map[string]any{"type": "ack", "event_id": id, "status": "queued", "summary": "stream-secret outcome"})
	if frame := readStream(t, c); frameType(frame) != "recorded" {
		t.Fatalf("ack %s", frame)
	}
	events, err := store.TaskEvents(context.Background(), storage.EventQuery{})
	if err != nil || len(events) != 1 || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].Summary != "[redacted] outcome" {
		t.Fatalf("receipt %+v, %v", events, err)
	}
	// A second consumer replays independently from the durable journal.
	other := connectStream(t, url)
	sendStream(t, other, map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "headless", "after_event_id": 0})
	if frame := readStream(t, other); frameType(frame) != "subscribed" {
		t.Fatalf("subscribe %s", frame)
	}
	if frame := readStream(t, other); frameType(frame) != "event" {
		t.Fatalf("replay %s", frame)
	}
}

func TestTaskConsumerStatusesReportDurableLeaseAndRedactCredential(t *testing.T) {
	_, streamURL := streamFixture(t)
	client := connectStream(t, streamURL)
	sendStream(t, client, map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "client-stream-secret"})
	if frame := readStream(t, client); frameType(frame) != "subscribed" {
		t.Fatalf("subscribe %s", frame)
	}
	url := "http" + strings.TrimPrefix(strings.TrimSuffix(streamURL, "/v1/tasks/stream"), "ws") + "/v1/task-events/consumers"
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer stream-secret")
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var statuses []storage.TaskConsumerStatus
	err = json.NewDecoder(response.Body).Decode(&statuses)
	if response.StatusCode != http.StatusOK || err != nil || len(statuses) != 1 ||
		statuses[0].ConsumerID != "client-[redacted]" || !statuses[0].Connected || statuses[0].LastSeenAt <= 0 {
		t.Fatalf("consumer status %d: %+v %v", response.StatusCode, statuses, err)
	}
}

func TestStreamAuthenticationAndStrictFrames(t *testing.T) {
	_, url := streamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(ctx, url, nil)
	if c != nil {
		c.CloseNow()
	}
	if res != nil {
		defer res.Body.Close()
	}
	if err == nil || res == nil || res.StatusCode != 401 {
		t.Fatalf("anonymous stream accepted: %v", err)
	}
	c = connectStream(t, url)
	for _, frame := range []any{
		map[string]any{"type": "heartbeat"},
		map[string]any{"type": "subscribe", "protocol_version": 2, "consumer_id": "x"},
		map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "x", "unexpected": true},
	} {
		sendStream(t, c, frame)
		if value := readStream(t, c); frameType(value) != "error" {
			t.Fatalf("invalid frame accepted: %s", value)
		}
	}
	sendStream(t, c, map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "x"})
	if value := readStream(t, c); frameType(value) != "subscribed" {
		t.Fatalf("valid subscription rejected: %s", value)
	}
	sendStream(t, c, map[string]any{"type": "heartbeat"})
	if value := readStream(t, c); frameType(value) != "heartbeat_ack" {
		t.Fatalf("heartbeat %s", value)
	}
}

// Ported payload behavior from Rust task_events.rs::payloads_bound_unicode_controls_and_redact_before_truncating.
func TestBoundEventUnicodeAndSecrets(t *testing.T) {
	text := strings.Repeat("stream-secret\n🦀", 30000)
	body, err := json.Marshal(map[string]any{"id": "task", "description": text})
	if err != nil {
		t.Fatal(err)
	}
	event := storage.TaskEvent{Task: body, Comment: body, Deliveries: []storage.EventDelivery{}}
	if err := boundTaskEvent(&event, "stream-secret"); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= streamFrameBytes-1024 || strings.Contains(string(encoded), "stream-secret") || !event.Truncated {
		t.Fatalf("event not bounded: bytes=%d, truncated=%v", len(encoded), event.Truncated)
	}
	if !json.Valid(encoded) {
		t.Fatal("invalid Unicode/JSON frame")
	}
	// Unknown-size object fields cannot be truncated by deleting canonical keys.
	large := map[string]any{}
	for i := 0; i < 1000; i++ {
		large[strconv.Itoa(i)] = strings.Repeat("x", 1000)
	}
	event.Task, _ = json.Marshal(large)
	if err := boundTaskEvent(&event, "stream-secret"); err == nil {
		t.Fatal("unbounded object escaped frame limit")
	}
}

func TestServerShutdownClosesWebSocket(t *testing.T) {
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler, err := NewHandler(store, "stream-secret")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, handler, time.Second) }()
	c := connectStream(t, "ws://"+listener.Addr().String()+"/v1/tasks/stream")
	sendStream(t, c, map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "shutdown"})
	if frame := readStream(t, c); frameType(frame) != "subscribed" {
		t.Fatalf("subscribe %s", frame)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WebSocket prevented shutdown")
	}
	readCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, _, err := c.Read(readCtx); err == nil {
		t.Fatal("WebSocket survived server shutdown")
	}
	// All handler cleanup completed before Serve returned, so the lease is reusable.
	if _, err := store.SubscribeTaskEvents(context.Background(), "successor", "shutdown", nil); err != nil {
		t.Fatal("stream lease leaked:", err)
	}
}

func TestStreamRejectsCrossOriginAndOversizedInput(t *testing.T) {
	_, url := streamFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer stream-secret"}, "Origin": []string{"https://unrelated.example"}}})
	if c != nil {
		c.CloseNow()
	}
	if res != nil {
		defer res.Body.Close()
	}
	if err == nil || res == nil || res.StatusCode != 403 {
		t.Fatalf("cross-origin stream accepted: %v", err)
	}
	c = connectStream(t, url)
	// The server must close rather than allocate an unbounded input frame.
	_ = c.Write(ctx, websocket.MessageText, []byte(strings.Repeat("x", streamFrameBytes+1)))
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestTaskMutationPublishesCommittedWebSocketSnapshot(t *testing.T) {
	store, url := streamFixture(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "live", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	c := connectStream(t, url)
	sendStream(t, c, map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": "live-worker", "after_event_id": 0})
	if frame := readStream(t, c); frameType(frame) != "subscribed" {
		t.Fatalf("subscribe %s", frame)
	}
	req, err := http.NewRequest("POST", "http"+strings.TrimSuffix(strings.TrimPrefix(url, "ws"), "/v1/tasks/stream")+"/v1/projects/live/tasks", strings.NewReader(`{"title":"Stream a committed task"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer stream-secret")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("create HTTP status %d", res.StatusCode)
	}
	var task storage.Task
	if err := json.NewDecoder(res.Body).Decode(&task); err != nil {
		t.Fatal(err)
	}
	frame := readStream(t, c)
	if frameType(frame) != "event" {
		t.Fatalf("mutation frame %s", frame)
	}
	var event storage.TaskEvent
	if err := json.Unmarshal(frame["event"], &event); err != nil {
		t.Fatal(err)
	}
	var snapshot storage.Task
	if err := json.Unmarshal(event.Task, &snapshot); err != nil {
		t.Fatal(err)
	}
	if event.Kind != "task_created" || snapshot.ID != task.ID || snapshot.Title != task.Title {
		t.Fatalf("wrong committed snapshot %+v", snapshot)
	}
	detail, err := store.Task(context.Background(), "live", snapshot.ID)
	if err != nil || detail == nil || detail.Task.ID != task.ID {
		t.Fatalf("stream observed uncommitted task: %+v, %v", detail, err)
	}
}
