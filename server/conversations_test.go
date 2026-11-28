package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestConversationAPIReplaysTaskTranscriptAndFencesWorker(t *testing.T) {
	s, h := taskAPI(t)
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Chat task", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	created := taskRequest(h, "POST", "/v1/projects/tasks/tasks/"+task.ID+"/conversations", "", actor)
	if created.Code != 200 {
		t.Fatalf("create %d %s", created.Code, created.Body)
	}
	var c storage.Conversation
	if err = json.Unmarshal(created.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/conversations/" + c.ID
	bound := taskRequest(h, "PUT", path+"/session", `{"native_session_id":"provider-session"}`, actor)
	if bound.Code != 200 {
		t.Fatalf("bind %d %s", bound.Code, bound.Body)
	}
	raw, _ := json.Marshal(map[string]any{"id": uuid.NewString(), "event": conversation.Event{Kind: "tool_call", SessionID: "provider-session", ToolCallID: "tool-1", Title: "Inspect", Status: "pending"}})
	for i := 0; i < 2; i++ {
		posted := taskRequest(h, "POST", path+"/events", string(raw), actor)
		if posted.Code != 200 {
			t.Fatalf("post %d %s", posted.Code, posted.Body)
		}
	}
	replay := taskRequest(h, "GET", path+"/events?after=0&limit=10", "", nil)
	var events []storage.ConversationEvent
	if replay.Code != 200 || json.Unmarshal(replay.Body.Bytes(), &events) != nil || len(events) != 1 || events[0].Event.ToolCallID != "tool-1" {
		t.Fatalf("replay %d %s", replay.Code, replay.Body)
	}
	other := *actor
	other.InstanceID = uuid.NewString()
	resume := taskRequest(h, "POST", path+"/resume", `{"previous_instance_id":"`+actor.InstanceID+`"}`, &other)
	if resume.Code != 200 {
		t.Fatalf("resume %d %s", resume.Code, resume.Body)
	}
	stale := taskRequest(h, "POST", path+"/events", string(raw), actor)
	if stale.Code != 409 {
		t.Fatalf("stale retry accepted %d %s", stale.Code, stale.Body)
	}
	foreign := taskRequest(h, "GET", "/v1/projects/foreign/conversations/"+c.ID, "", nil)
	if foreign.Code != 404 {
		t.Fatalf("cross-project read %d %s", foreign.Code, foreign.Body)
	}
}

func TestConversationAPIRejectsMalformedRequests(t *testing.T) {
	s, h := taskAPI(t)
	task, err := s.InsertTask(context.Background(), storage.TaskInsert{Project: "tasks", Title: "Chat task", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	c, err := s.CreateConversation(context.Background(), "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/conversations/" + c.ID
	if res := taskRequest(h, http.MethodPost, "/v1/projects/tasks/conversations", "", nil); res.Code != http.StatusBadRequest {
		t.Fatalf("taskless create without origin: %d %s", res.Code, res.Body)
	}
	for _, test := range []struct {
		method, suffix, body string
		actor                *agent.Origin
		code                 int
	}{
		{"PUT", "/session", `{"native_session_id":"native"}`, nil, 400},
		{"PUT", "/session", `null`, actor, 400},
		{"PUT", "/session", `{"native_session_id":""}`, actor, 400},
		{"PUT", "/session", `{"native_session_id":"native","unknown":true}`, actor, 400},
		{"POST", "/resume", `{}`, actor, 400},
		{"POST", "/events", `{"id":"bad","event":{}}`, actor, 400},
		{"GET", "/events?after=-1", "", nil, 400},
		{"GET", "/events?limit=1001", "", nil, 400},
		{"GET", "/events?after=0&after=1", "", nil, 400},
		{"GET", "/events?unknown=1", "", nil, 400},
	} {
		res := taskRequest(h, test.method, path+test.suffix, test.body, test.actor)
		if res.Code != test.code {
			t.Fatalf("%s %s: %d %s", test.method, test.suffix, res.Code, res.Body)
		}
	}
}

func TestConversationStreamReplaysReconnectsAndDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, h := taskAPI(t)
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Streaming chat", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindConversationSession(ctx, "tasks", c.ID, "native", actor); err != nil {
		t.Fatal(err)
	}
	appendText := func(text string) {
		t.Helper()
		if _, err := s.AppendConversationEvent(ctx, "tasks", c.ID, uuid.NewString(), conversation.Event{Kind: "text_delta", SessionID: "native", Text: text}, actor); err != nil {
			t.Fatal(err)
		}
	}
	appendText("before connect")
	srv := httptest.NewServer(h)
	defer srv.Close()
	shutdown := h.(interface{ ShutdownStreams(context.Context) error })
	defer shutdown.ShutdownStreams(ctx)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/projects/tasks/conversations/" + c.ID + "/stream"
	dial := func(after string) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, url+"?after="+after, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer task-key"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		ready := readStream(t, conn)
		if string(ready["type"]) != `"ready"` || string(ready["version"]) != "1" {
			t.Fatalf("ready %v", ready)
		}
		return conn
	}
	readEvent := func(conn *websocket.Conn, sequence int64, text string) {
		t.Helper()
		frame := readStream(t, conn)
		var event storage.ConversationEvent
		if string(frame["type"]) != `"event"` || json.Unmarshal(frame["event"], &event) != nil || event.Sequence != sequence || event.Event.Text != text {
			t.Fatalf("event %v", frame)
		}
	}
	first := dial("0")
	readEvent(first, 1, "before connect")
	appendText("live")
	readEvent(first, 2, "live")
	first.CloseNow()
	appendText("during disconnect")
	resumed := dial("2")
	readEvent(resumed, 3, "during disconnect")
	if err := shutdown.ShutdownStreams(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resumed.Read(ctx); err == nil {
		t.Fatal("conversation stream survived server shutdown")
	}
}
