package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
)

func TestConversationCommandsHTTPAndStreamShareDurableState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, h := taskAPI(t)
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Remote agent", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer h.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx)
	path := "/v1/projects/tasks/conversations/" + c.ID
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path+"/stream?after=0&after_commands=0", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer task-key"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ready := readStream(t, conn)
	if string(ready["type"]) != `"ready"` {
		t.Fatalf("ready %v", ready)
	}
	id := uuid.NewString()
	body, _ := json.Marshal(map[string]any{"id": id, "command": conversation.Command{Kind: "prompt", Text: "Inspect the task"}})
	queued := taskRequest(h, "POST", path+"/commands", string(body), nil)
	if queued.Code != 200 {
		t.Fatalf("enqueue %d %s", queued.Code, queued.Body)
	}
	var revision int64
	for _, step := range []struct{ operation, state string }{
		{"", "queued"}, {"claim", "claimed"}, {"result", "done"},
	} {
		if step.operation != "" {
			body := ""
			if step.operation == "result" {
				body = `{"state":"done"}`
			}
			response := taskRequest(h, "POST", path+"/commands/"+id+"/"+step.operation, body, actor)
			if response.Code != 200 {
				t.Fatalf("%s %d %s", step.operation, response.Code, response.Body)
			}
		}
		frame := readStream(t, conn)
		var command storage.ConversationCommand
		if string(frame["type"]) != `"command"` || json.Unmarshal(frame["command"], &command) != nil || command.ID != id || command.State != step.state || command.Revision <= revision {
			t.Fatalf("stream step %s: %v", step.state, frame)
		}
		revision = command.Revision
	}
	list := taskRequest(h, "GET", path+"/commands?after=0&limit=10", "", nil)
	var commands []storage.ConversationCommand
	if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &commands) != nil || len(commands) != 1 || commands[0].State != "done" {
		t.Fatalf("list %d %s", list.Code, list.Body)
	}
	changes := taskRequest(h, "GET", path+"/commands/changes?after=0&limit=10", "", nil)
	if changes.Code != 200 || json.Unmarshal(changes.Body.Bytes(), &commands) != nil || len(commands) != 3 || commands[2].Revision != revision {
		t.Fatalf("changes %d %s", changes.Code, changes.Body)
	}
	// Repeated commands retain their original result and do not stream new work.
	retried := taskRequest(h, "POST", path+"/commands", string(body), nil)
	if retried.Code != 200 || !strings.Contains(retried.Body.String(), `"state":"done"`) {
		t.Fatalf("retry %d %s", retried.Code, retried.Body)
	}
	conn.CloseNow()
	resumed, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path+"/stream?after_commands="+strconv.FormatInt(revision, 10), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer task-key"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.CloseNow()
	if ready := readStream(t, resumed); string(ready["type"]) != `"ready"` {
		t.Fatalf("resume ready %v", ready)
	}
	next, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Next task step"})
	if err != nil {
		t.Fatal(err)
	}
	frame := readStream(t, resumed)
	var received storage.ConversationCommand
	if json.Unmarshal(frame["command"], &received) != nil || received.ID != next.ID || received.Revision <= revision {
		t.Fatalf("command cursor replayed old intent: %v", frame)
	}

}

func TestConversationCommandAPIRequiresWorkerForClaimAndResult(t *testing.T) {
	s, h := taskAPI(t)
	ctx := context.Background()
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Remote agent", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	command, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, id, conversation.Command{Kind: "prompt", Text: "Inspect"})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/tasks/conversations/" + c.ID + "/commands/" + command.ID
	anonymous := taskRequest(h, "POST", path+"/claim", "", nil)
	if anonymous.Code != 400 {
		t.Fatalf("anonymous claim %d %s", anonymous.Code, anonymous.Body)
	}
	other := *actor
	other.InstanceID = uuid.NewString()
	stale := taskRequest(h, "POST", path+"/claim", "", &other)
	if stale.Code != 409 {
		t.Fatalf("foreign claim %d %s", stale.Code, stale.Body)
	}
	premature := taskRequest(h, "POST", path+"/result", `{"state":"done"}`, actor)
	if premature.Code != 409 {
		t.Fatalf("unclaimed completion %d %s", premature.Code, premature.Body)
	}
	invalid := taskRequest(h, "POST", path+"/result", `{"state":"unknown"}`, actor)
	if invalid.Code != 400 {
		t.Fatalf("invalid result %d %s", invalid.Code, invalid.Body)
	}
}

func TestPendingTaskConversationsAreScopedToAuthenticatedPrincipal(t *testing.T) {
	ctx := context.Background()
	s, _ := taskAPI(t)
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "tasks", Title: "Follow up", Reporter: "user"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := apiActor()
	c, err := s.CreateConversation(ctx, "tasks", task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSessionWithPrincipal(ctx, "tasks", c.ID, "provider-session", actor, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSessionWithPrincipal(ctx, "tasks", c.ID, "provider-session", actor, "node-b"); !errors.Is(err, storage.ErrTaskOwnership) {
		t.Fatalf("foreign principal rebind err=%v", err)
	}
	if _, err := s.EnqueueConversationCommand(ctx, "tasks", c.ID, uuid.NewString(), conversation.Command{Kind: "prompt", Text: "Again"}); err != nil {
		t.Fatal(err)
	}
	// Live presence ownership of the session moving to another enrolled client
	// must not transfer access to conversations bound under the first one.
	report := storage.PresenceReport{Source: "native:node-b", Sequence: 1, Agents: []storage.PresenceAgent{{ProjectIdent: "tasks", Origin: *actor}}}
	if accepted, err := s.ReportPresenceWithPrincipal(ctx, report, time.Now(), "node-b"); err != nil || !accepted {
		t.Fatalf("presence accepted=%v err=%v", accepted, err)
	}
	a := newAPI(s, "task-key")
	mux := http.NewServeMux()
	a.registerConversationCommandRoutes(mux)
	pending := func(principal string) []string {
		r := httptest.NewRequest("GET", "/v1/projects/tasks/worker-task-conversations/pending", nil)
		r.Header.Set("X-Agent-Session-Id", actor.SessionID)
		r.Header.Set("X-Agent-Instance-Id", actor.InstanceID)
		r.Header.Set("X-Agent-Provider", actor.Provider)
		r.Header.Set("X-Agent-OS", actor.OS)
		r = r.WithContext(context.WithValue(r.Context(), authenticatedPrincipalContextKey{}, principal))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var ids []string
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ids) != nil {
			t.Fatalf("pending as %q: %d %s", principal, w.Code, w.Body)
		}
		return ids
	}
	if ids := pending("node-a"); len(ids) != 1 || ids[0] != c.ID {
		t.Fatalf("owning client ids=%v", ids)
	}
	if ids := pending("node-b"); len(ids) != 0 {
		t.Fatalf("different enrolled client listed another session's conversations: %v", ids)
	}
}
