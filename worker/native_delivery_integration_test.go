package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/chat"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/storage"
)

func TestNativeDeliveryUsesOneAdoptionPromptAndDeliversReminderAfterTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, "", false, http.DefaultTransport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	definitions := append(append(storage.RouterPresenceEventDefinitions(), storage.RouterTaskEventDefinitions()...), plugins.ClockDefinition(), plugins.ClientHeartbeatDefinition())
	definitions = append(definitions, plugins.ActivityEventDefinitions()...)
	definitions = append(definitions, plugins.NotificationEventDefinitions()...)
	runtime, err := router.NewRuntime(remote, remote.EventBus(), definitions, []router.Plugin{
		plugins.PresencePlugin(remote), plugins.AssignmentPlugin(remote), plugins.NativeDeliveryPlugin(remote),
	}, router.RunConfig{Capacity: 8})
	if err != nil {
		t.Fatal(err)
	}
	routerCtx, stopRouter := context.WithCancel(ctx)
	routerDone := make(chan error, 1)
	go func() { routerDone <- runtime.Run(routerCtx) }()
	defer func() {
		stopRouter()
		select {
		case err := <-routerDone:
			if err != nil {
				t.Errorf("native delivery runtime shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("native delivery runtime did not stop")
		}
	}()
	opened := make(chan storage.WorkerRun, 1)
	sessions := make(chan *poolFixtureSession, 1)
	release := make(chan struct{})
	open := func(openCtx context.Context, run storage.WorkerRun, sink conversation.Sink) (Session, error) {
		if err := sink(openCtx, conversation.Event{Kind: "session_ready", SessionID: "pool-native"}); err != nil {
			return nil, err
		}
		session := &poolFixtureSession{done: make(chan struct{}), sink: sink, started: make(chan string, 2), release: release}
		opened <- run
		sessions <- session
		return session, nil
	}
	poolCtx, stopPool := context.WithCancel(ctx)
	poolDone := make(chan error, 1)
	go func() {
		poolDone <- RunPool(poolCtx, PoolConfig{Store: local, Client: client, PoolID: pool.ID, Open: open, PollInterval: 10 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond})
	}()
	defer func() {
		stopPool()
		select {
		case err := <-poolDone:
			if err != nil {
				t.Errorf("native delivery pool shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("native delivery pool did not stop")
		}
	}()
	wait := func(label string, condition func() (bool, error)) {
		t.Helper()
		for {
			ready, err := condition()
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			if ready {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s", label)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait("pool source advertisement", func() (bool, error) {
		agents, err := remote.Presence(ctx, pool.Project)
		return len(agents) == 1 && agents[0].Origin == slot.Owner && agents[0].AutoAssign, err
	})
	// Evaluate the ordinary idle window without sleeping or starting work yet.
	if err := remote.ReconcilePresence(ctx, time.Now().Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "Native notification target", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var run storage.WorkerRun
	select {
	case run = <-opened:
	case <-ctx.Done():
		t.Fatal("native assignment was not opened")
	}
	session := <-sessions
	select {
	case <-session.started:
	case <-ctx.Done():
		t.Fatal("initial assignment prompt did not start")
	}
	wait("current resumed native receiver", func() (bool, error) {
		agents, err := remote.Presence(ctx, pool.Project)
		if err != nil {
			return false, err
		}
		for _, agent := range agents {
			if agent.Origin == run.Owner && agent.State != "gone" && agent.Busy {
				return true, nil
			}
		}
		return false, nil
	})
	var assigned router.Event
	wait("captured assignment delivery result", func() (bool, error) {
		events, err := remote.RouterEvents(ctx, 0, 200)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.ID == plugins.NotifyEventID && event.Source == "plugin:assignment" {
				assigned = event
			}
		}
		for _, event := range events {
			if event.ID == plugins.NotifyResultEventID && event.Correlation != nil && *event.Correlation == assigned.UID {
				var result map[string]string
				if err := json.Unmarshal(event.Payload, &result); err != nil {
					return false, err
				}
				if result["status"] != "delivered" || event.Source != pool.ID+"/"+slot.ID {
					t.Fatalf("assignment result lost captured authority/effect: %+v %s", event, event.Payload)
				}
				return true, nil
			}
		}
		return false, nil
	})
	commands, err := remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	if err != nil || len(commands) != 1 || session.promptCount.Load() != 1 {
		t.Fatalf("notification duplicated adoption prompt: %+v %v", commands, err)
	}
	detail, err := remote.Task(ctx, pool.Project, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"target": run.Owner.SessionID, "project": pool.Project, "mode": "assigned", "require_idle": true, "reminder": true,
		"receiver": map[string]string{"session_id": run.Owner.SessionID, "instance_id": run.Owner.InstanceID, "generation": run.Owner.InstanceID, "project": pool.Project},
		"heading":  "Task update requested", "instructions": "Post a progress update without changing task ownership.",
		"event": map[string]any{"kind": "task_created", "task": detail.Task, "comment": nil, "origin": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	uid := uuid.NewString()
	projection := chat.New()
	const draft = "An unsent human message must survive the server notification."
	projection.SetDraft(draft)
	request := router.EmitRequest{UID: uid, ID: plugins.NotifyEventID, Version: 1, Source: "plugin:reminder", Payload: payload}
	reminder, err := remote.EmitRouterEvent(ctx, runtime.Registry(), request)
	if err != nil || !reminder.Stored {
		t.Fatalf("reminder admission: %+v %v", reminder, err)
	}
	wait("busy provider turn postpones reminder", func() (bool, error) {
		deliveries, err := remote.RouterDeliveries(ctx, reminder.Event.Seq)
		for _, delivery := range deliveries {
			if delivery.Plugin == "native-delivery" && delivery.State == "retrying" {
				return true, err
			}
		}
		return false, err
	})
	commands, err = remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	if err != nil || len(commands) != 1 {
		t.Fatalf("reminder interrupted a provider turn: %+v %v", commands, err)
	}
	close(release)
	var notificationText string
	select {
	case text := <-session.started:
		notificationText = text
		if !strings.Contains(text, "[amux notification]") || !strings.Contains(text, task.ID) || !strings.Contains(text, "Post a progress update") {
			t.Fatalf("native reminder omitted context: %s", text)
		}
	case <-ctx.Done():
		t.Fatal("owned-task Busy blocked the idle provider's reminder")
	}
	wait("reminder result", func() (bool, error) {
		events, err := remote.RouterEvents(ctx, 0, 200)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.ID == plugins.NotifyResultEventID && event.Correlation != nil && *event.Correlation == uid {
				var result map[string]string
				if err := json.Unmarshal(event.Payload, &result); err != nil {
					return false, err
				}
				if result["status"] != "delivered" || event.Source != pool.ID+"/"+slot.ID {
					t.Fatalf("reminder result authority: %+v %s", event, event.Payload)
				}
				return true, nil
			}
		}
		return false, nil
	})
	if replay, err := remote.EmitRouterEvent(ctx, runtime.Registry(), request); err != nil || !replay.Replayed {
		t.Fatalf("notification replay: %+v %v", replay, err)
	}
	commands, err = remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	if err != nil || len(commands) != 2 || session.promptCount.Load() != 2 || commands[1].ID != uid || commands[1].Command.Text != notificationText {
		t.Fatalf("replay duplicated reminder intent: %+v %v", commands, err)
	}
	wait("notification appears in shared chat without changing the human draft", func() (bool, error) {
		events, err := remote.ConversationEvents(ctx, pool.Project, run.ConversationID, 0, 100)
		if err != nil {
			return false, err
		}
		seen := false
		for _, event := range events {
			if err := projection.Apply(event); err != nil {
				return false, err
			}
			seen = seen || event.Event.Kind == "user_message" && event.Event.MessageID == uid
		}
		if projection.Snapshot().Draft != draft {
			t.Fatal("notification changed the human's unsent draft")
		}
		return seen, nil
	})
	notices := 0
	for _, item := range projection.Snapshot().Items {
		if strings.Contains(item.Text, "[amux notification]") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("chat replay shows %d copies of the notification", notices)
	}
	current, err := remote.Task(ctx, pool.Project, task.ID)
	if err != nil || current.Task.Status != "in_progress" || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != run.Owner {
		t.Fatalf("notification changed task ownership: %+v %v", current, err)
	}
}
