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
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/server"
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
	for _, id := range []string{"notifier", "reminder"} {
		if err := remote.SetPluginEnabled(ctx, id, false); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := server.NewGatewayFISM(remote, 7, 7)
	if err != nil {
		t.Fatal(err)
	}
	routerCtx, stopRouter := context.WithCancel(ctx)
	routerDone := make(chan error, 1)
	ready := make(chan struct{})
	go func() { routerDone <- runtime.Run(routerCtx, ready) }()
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
	open := func(openCtx context.Context, run storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
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
	if _, err := remote.AcceptObservation(ctx, runtime.Registry(), fism.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)}); err != nil {
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
	var assigned fism.Event
	wait("captured assignment delivery result", func() (bool, error) {
		events, err := remote.RouterEvents(ctx, 0, 200)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.ID == controllers.NotifyEventID && event.Source == "plugin:assignment" {
				assigned = event
			}
		}
		for _, event := range events {
			if event.ID == controllers.NotifyResultEventID && event.Correlation != nil && *event.Correlation == assigned.UID {
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
	request := fism.EmitRequest{UID: uid, ID: controllers.NotifyEventID, Version: 1, Source: "plugin:reminder", Payload: payload}
	reminder, err := remote.AcceptObservation(ctx, runtime.Registry(), request)
	if err != nil || !reminder.Stored {
		t.Fatalf("reminder admission: %+v %v", reminder, err)
	}
	wait("busy provider turn postpones reminder", func() (bool, error) {
		pending, err := remote.PendingNotifications(ctx, 0, 200)
		for _, notice := range pending {
			if notice.Event.UID == reminder.Event.UID {
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
	for notificationText == "" {
		select {
		case text := <-session.started:
			if strings.Contains(text, "Post a progress update") {
				notificationText = text
				if !strings.Contains(text, "[amux notification]") || !strings.Contains(text, task.ID) {
					t.Fatalf("native reminder omitted context: %s", text)
				}
			}
		case <-ctx.Done():
			t.Fatal("owned-task Busy blocked the idle provider's reminder")
		}
	}

	wait("reminder result", func() (bool, error) {
		events, err := remote.RouterEvents(ctx, 0, 200)
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.ID == controllers.NotifyResultEventID && event.Correlation != nil && *event.Correlation == uid {
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
	if replay, err := remote.AcceptObservation(ctx, runtime.Registry(), request); err != nil || !replay.Replayed {
		t.Fatalf("notification replay: %+v %v", replay, err)
	}
	commands, err = remote.ConversationCommands(ctx, pool.Project, run.ConversationID, 0, 100)
	matches := 0
	for _, command := range commands {
		if command.ID == uid {
			matches++
			if command.Command.Text != notificationText {
				t.Fatalf("reminder command content changed: %+v", command)
			}
		}
	}
	if err != nil || matches != 1 {
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
		if item.Text == notificationText {
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
