package storage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

type gatedReminderPresence struct {
	*Store
	once              sync.Once
	entered, released chan struct{}
}

func (p *gatedReminderPresence) ReconcileRouterPresence(ctx context.Context, registry *router.Registry, event router.Event) error {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.released:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.Store.ReconcileRouterPresence(ctx, registry, event)
}

func TestReminderRuntimeRejectsSupersededLifetimeAfterPresenceDependency(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	owner := taskActor()
	task := makeTask(t, s, owner)
	if _, err := s.TransitionTask(ctx, "tasks", task.ID, "in_progress", owner); err != nil {
		t.Fatal(err)
	}
	agent := PresenceAgent{ProjectIdent: "tasks", Origin: *owner, Busy: true}
	oldRun, newRun := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native-reminder", Sequence: 1, ClientRun: &oldRun, Agents: []PresenceAgent{agent}}, now); err != nil || !ok {
		t.Fatalf("first lifetime: %v %v", ok, err)
	}
	gate := &gatedReminderPresence{Store: s, entered: make(chan struct{}), released: make(chan struct{})}
	definitions := append(nativePresenceDefinitions(), plugins.NotificationEventDefinitions()...)
	runtime, err := router.NewRuntime(s, s.EventBus(), definitions, []router.Plugin{plugins.PresencePlugin(gate), plugins.ReminderPlugin(s), {
		Definition: router.PluginDefinition{ID: "notification-sink", Subscriptions: []router.Subscription{{Event: plugins.NotifyEventID, MinVersion: 1, MaxVersion: 1}}},
	}}, router.RunConfig{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainRouterHeartbeatOutbox(ctx, runtime.Registry(), 200); err != nil {
		t.Fatal(err)
	}
	events, err := s.RouterEvents(ctx, 0, 20)
	if err != nil || len(events) != 1 {
		t.Fatalf("first heartbeat: %+v %v", events, err)
	}
	old := events[0]
	stop := runRouterFixture(t, runtime)
	awaitRouterSignal(t, gate.entered)
	deliveries, err := s.RouterDeliveries(ctx, old.Seq)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		if delivery.Plugin == "reminder" && delivery.State != "pending" {
			t.Fatalf("reminder bypassed Presence: %+v", deliveries)
		}
	}
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native-reminder", Sequence: 2, ClientRun: &newRun, Agents: []PresenceAgent{agent}}, now.Add(time.Millisecond)); err != nil || !ok {
		t.Fatalf("replacement lifetime: %v %v", ok, err)
	}
	if _, err := s.DrainRouterHeartbeatOutbox(ctx, runtime.Registry(), 200); err != nil {
		t.Fatal(err)
	}
	events, err = s.RouterEvents(ctx, 0, 20)
	if err != nil || len(events) != 2 {
		t.Fatalf("replacement heartbeat: %+v %v", events, err)
	}
	current := events[1]
	close(gate.released)
	waitRouterDeliveryState(t, s, old.Seq, "reminder", "completed")
	waitRouterDeliveryState(t, s, current.Seq, "reminder", "completed")
	stop()
	if err := s.ReconcileRouterReminders(ctx, runtime.Registry(), current); err != nil {
		t.Fatal(err)
	}
	var notices int
	var correlation, savedRun string
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(MAX(correlation),'') FROM router_events WHERE id='agent.notify'").Scan(&notices, &correlation); err != nil || notices != 1 || correlation != current.UID {
		t.Fatalf("startup notices=%d cause=%s current=%s error=%v", notices, correlation, current.UID, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT client_run FROM reminder_restarts WHERE source='native-reminder' AND session_id=? AND instance_id=?", owner.SessionID, owner.InstanceID).Scan(&savedRun); err != nil || savedRun != newRun {
		t.Fatalf("restart receipt=%s error=%v", savedRun, err)
	}
	unchanged, err := s.Task(ctx, "tasks", task.ID)
	if err != nil || unchanged.Task.Status != "in_progress" || unchanged.Task.OwnerOrigin == nil || *unchanged.Task.OwnerOrigin != *owner {
		t.Fatalf("reminder changed ownership: %+v %v", unchanged, err)
	}
}
