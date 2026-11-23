package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/storage"
)

// Receiving a heartbeat through the authenticated generic stream does not turn
// its client-selected agent identity into an authenticated native execution.
func TestRemoteHeartbeatCannotAuthorizeNativeNotification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote-authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "remote-authority", ChannelName: "remote-authority", RoomID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	actor := &storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}
	task, err := s.InsertTask(ctx, storage.TaskInsert{Project: "remote-authority", Title: "Observe without executing", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation(ctx, task.ProjectIdent, task.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversationSession(ctx, task.ProjectIdent, c.ID, "native-fixture", actor); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Task(ctx, task.ProjectIdent, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defs := append(plugins.NotificationEventDefinitions(), plugins.ClientHeartbeatDefinition(), plugins.ClockDefinition())
	defs = append(defs, plugins.ActivityEventDefinitions()...)
	defs = append(defs, storage.RouterPresenceEventDefinitions()...)
	reg, err := router.NewRegistry(defs, []router.PluginDefinition{
		plugins.PresencePlugin(s).Definition,
		plugins.NativeDeliveryPlugin(s).Definition,
		{ID: "result-observer", Subscriptions: []router.Subscription{{Event: plugins.NotifyResultEventID, MinVersion: 1, MaxVersion: 1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandlerWithRemote(s, "stream-secret", RemoteConfig{Context: ctx, Registry: reg, Forwarded: []string{plugins.NotifyEventID}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer h.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(ctx)
	conn := connectStream(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/events/stream")
	defer conn.CloseNow()
	reportRaw := func(agents []map[string]any) {
		t.Helper()
		sendStream(t, conn, map[string]any{"type": "emit", "id": plugins.ClientHeartbeatID, "version": 1, "payload": map[string]any{
			"principal": "api-key", "source": "native:forged", "agents": agents,
		}})
		response := readStream(t, conn)
		if frameType(response) != "emitted" {
			t.Fatalf("heartbeat response: %s", response)
		}
		var emittedUID string
		if err := json.Unmarshal(response["uid"], &emittedUID); err != nil {
			t.Fatal(err)
		}
		events, err := s.RouterEvents(ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var heartbeat *router.Event
		for i := range events {
			if events[i].UID == emittedUID {
				heartbeat = &events[i]
				break
			}
		}
		if heartbeat == nil || heartbeat.Source != "client:api-key" {
			t.Fatalf("server-derived source: %+v", heartbeat)
		}
		if err := s.ReconcilePresenceEvent(ctx, reg, *heartbeat); err != nil {
			t.Fatal(err)
		}
	}
	reportRaw([]map[string]any{{"project": task.ProjectIdent, "session_id": actor.SessionID, "instance_id": actor.InstanceID, "provider": actor.Provider, "os": actor.OS, "auto_assign": true}})
	notice, err := json.Marshal(map[string]any{"target": actor.SessionID, "project": task.ProjectIdent, "mode": "recipient", "require_idle": true, "event": map[string]any{"kind": "task_commented", "task": detail.Task}})
	if err != nil {
		t.Fatal(err)
	}
	emitNotice := func() router.Event {
		t.Helper()
		r, err := s.EmitRouterEvent(ctx, reg, router.EmitRequest{ID: plugins.NotifyEventID, Version: 1, Source: "plugin:notifier", Payload: notice})
		if err != nil || !r.Stored {
			t.Fatalf("emit notice: %+v %v", r, err)
		}
		return r.Event
	}
	if _, err := s.ReconcileNativeDelivery(ctx, reg, emitNotice()); err != nil {
		t.Fatal(err)
	}
	commands, err := s.ConversationCommands(ctx, task.ProjectIdent, c.ID, 0, 100)
	if err != nil || len(commands) != 0 {
		t.Fatalf("raw observation queued native work: %+v %v", commands, err)
	}
	// A fresh authenticated native report is the positive authority control.
	// First withdraw the raw source's live identity; ownership cannot be stolen.
	reportRaw([]map[string]any{})
	accepted, err := s.ReportPresenceWithPrincipal(ctx, storage.PresenceReport{Source: "native:real", Sequence: 1, Agents: []storage.PresenceAgent{{ProjectIdent: task.ProjectIdent, Origin: *actor, Busy: true}}}, time.Now().UTC(), "api-key")
	if err != nil || !accepted {
		t.Fatalf("native report: %t %v", accepted, err)
	}
	if pending, err := s.ReconcileNativeDelivery(ctx, reg, emitNotice()); err != nil || pending {
		t.Fatalf("native delivery: %t %v", pending, err)
	}
	commands, err = s.ConversationCommands(ctx, task.ProjectIdent, c.ID, 0, 100)
	if err != nil || len(commands) != 1 {
		t.Fatalf("native control did not queue exactly once: %+v %v", commands, err)
	}
}
