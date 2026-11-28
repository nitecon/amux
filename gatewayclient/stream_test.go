package gatewayclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestConversationStreamDeliversTranscriptAndCommandCursors(t *testing.T) {
	stop := errors.New("projection committed")
	owner := agent.Origin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		wsjson.Write(ctx, c, map[string]any{"type": "ready", "version": 1, "conversation_id": "thread", "owner": owner})
		sequence := int64(1)
		if r.URL.Query().Get("after") == "1" {
			if r.URL.Query().Get("after_commands") != "7" {
				t.Error("command revision not sent on reconnect")
			}
			sequence = 2
		}
		wsjson.Write(ctx, c, map[string]any{"type": "event", "version": 1, "event": storage.ConversationEvent{ConversationID: "thread", Sequence: sequence, Event: conversation.Event{Kind: "text_delta", Text: "hello"}}})
		if sequence == 1 {
			wsjson.Write(ctx, c, map[string]any{"type": "command", "version": 1, "command": storage.ConversationCommand{ConversationID: "thread", Revision: 7}})
		}
		closed := c.CloseRead(ctx)
		<-closed.Done()
	}))
	defer srv.Close()
	client, err := New(srv.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cursor := Cursor{}
	err = client.Stream(ctx, "demo", "thread", cursor, func(frame Frame) error {
		if frame.Type == "event" {
			if frame.Event.Event.Text != "hello" {
				t.Fatal("text lost")
			}
			cursor.Events = frame.Event.Sequence
		}
		if frame.Type == "command" {
			cursor.Commands = frame.Command.Revision
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || cursor.Events != 1 || cursor.Commands != 7 {
		t.Fatalf("projection cursor %+v: %v", cursor, err)
	}
	err = client.Stream(ctx, "demo", "thread", cursor, func(frame Frame) error {
		if frame.Type == "event" {
			cursor.Events = frame.Event.Sequence
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || cursor.Events != 2 {
		t.Fatalf("resume %+v: %v", cursor, err)
	}
}
func TestConversationStreamRejectsGapBeforeProjection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		wsjson.Write(r.Context(), c, map[string]any{"type": "ready", "version": 1, "conversation_id": "thread", "owner": agent.Origin{}})
		wsjson.Write(r.Context(), c, map[string]any{"type": "event", "version": 1, "event": storage.ConversationEvent{ConversationID: "thread", Sequence: 2}})
		closed := c.CloseRead(r.Context())
		<-closed.Done()
	}))
	defer srv.Close()
	client, err := New(srv.URL, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	projected := false
	err = client.Stream(ctx, "demo", "thread", Cursor{}, func(frame Frame) error {
		if frame.Type == "event" {
			projected = true
		}
		return nil
	})
	if err == nil || Retryable(err) || projected {
		t.Fatalf("gap projected=%v error=%v", projected, err)
	}
}
