package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func assignmentNotifyObserver() router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{ID: "assignment-notify-observer", Subscriptions: []router.Subscription{{Event: plugins.NotifyEventID, MinVersion: 1, MaxVersion: 1}}},
		Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			return router.Outcome{Kind: router.Continue}
		}},
	}
}

type gatedAssignmentPresence struct {
	*Store
	entered, released chan struct{}
}

func (p *gatedAssignmentPresence) ReconcileRouterPresence(ctx context.Context, r *router.Registry, event router.Event) error {
	close(p.entered)
	select {
	case <-p.released:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.Store.ReconcileRouterPresence(ctx, r, event)
}

func assignmentPluginFixture(t *testing.T) (*Store, PresenceAgent, *Task, *Task) {
	t.Helper()
	s := presenceStore(t, filepath.Join(t.TempDir(), "assignment-plugin.db"))
	ctx := context.Background()
	old := time.Now().UTC().Add(-31 * time.Second)
	activity := old.UnixMilli()
	agent := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	agent.AutoAssign = true
	agent.ReviewContextClear = true
	agent.LastActivityAt = &activity
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "worker:assignment", Sequence: 1, Agents: []PresenceAgent{agent}}, old); err != nil || !ok {
		t.Fatalf("report %v %v", ok, err)
	}
	first, err := s.InsertTask(ctx, TaskInsert{Project: "presence", Title: "Oldest work", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.InsertTask(ctx, TaskInsert{Project: "presence", Title: "Later work", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	setTaskCreatedAt(t, s, first, 1)
	setTaskCreatedAt(t, s, second, 2)
	return s, agent, first, second
}

func TestAssignmentPluginWaitsForPresenceThenPreservesFIFOAndNativeAdoption(t *testing.T) {
	s, agent, first, second := assignmentPluginFixture(t)
	ctx := context.Background()
	gate := &gatedAssignmentPresence{Store: s, entered: make(chan struct{}), released: make(chan struct{})}
	definitions := append(append(nativePresenceDefinitions(), RouterTaskEventDefinitions()...), plugins.NotificationEventDefinitions()...)
	runtime, err := router.NewRuntime(s, s.EventBus(), definitions, []router.Plugin{plugins.PresencePlugin(gate), plugins.AssignmentPlugin(s), assignmentNotifyObserver()}, router.RunConfig{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	clock, err := s.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	awaitRouterSignal(t, gate.entered)
	pending, err := s.RouterDeliveries(ctx, clock.Event.Seq)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range pending {
		if delivery.Plugin == "assignment" && delivery.State != "pending" {
			t.Fatalf("assignment bypassed presence: %+v", pending)
		}
	}
	before, err := readTask(ctx, s.db, "presence", first.ID)
	if err != nil || before.OwnerOrigin != nil {
		t.Fatalf("assignment ran before presence: %+v %v", before, err)
	}
	close(gate.released)
	waitRouterDeliveryState(t, s, clock.Event.Seq, "assignment", "completed")
	assigned, err := readTask(ctx, s.db, "presence", first.ID)
	if err != nil || assigned.Status != "todo" || assigned.StartedAt != nil || assigned.OwnerOrigin == nil || *assigned.OwnerOrigin != agent.Origin {
		t.Fatalf("oldest native assignment %+v %v", assigned, err)
	}
	later, err := readTask(ctx, s.db, "presence", second.ID)
	if err != nil || later.Status != "todo" || later.OwnerOrigin != nil {
		t.Fatalf("one session received multiple tasks %+v %v", later, err)
	}
	stop()
	if err := s.ReconcileRouterAssignments(ctx, clock.Event); err != nil {
		t.Fatal(err)
	}
	run, err := s.AdoptWorkerAssignment(ctx, "presence", &agent.Origin)
	if err != nil || run == nil || run.Task.Status != "in_progress" || run.Task.StartedAt == nil {
		t.Fatalf("worker adoption did not start reserved task: %+v %v", run, err)
	}
	again, err := s.AdoptWorkerAssignment(ctx, "presence", &agent.Origin)
	if err != nil || run.RunID != again.RunID || run.Conversation.ID != again.Conversation.ID {
		t.Fatalf("native adoption duplicated run: %+v %+v %v", run, again, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversations", int64(1))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_conversation_commands WHERE kind='prompt'", int64(1))
	later, err = readTask(ctx, s.db, "presence", second.ID)
	if err != nil || later.OwnerOrigin != nil {
		t.Fatalf("replay allocated second task %+v %v", later, err)
	}
}

func TestAssignmentPluginDisabledStopsAndNewTickHonorsEnable(t *testing.T) {
	s, agent, first, _ := assignmentPluginFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('plugin.assignment.enabled','false')`); err != nil {
		t.Fatal(err)
	}
	definitions := append(append(nativePresenceDefinitions(), RouterTaskEventDefinitions()...), plugins.NotificationEventDefinitions()...)
	runtime, err := router.NewRuntime(s, s.EventBus(), definitions, []router.Plugin{plugins.PresencePlugin(s), plugins.AssignmentPlugin(s), assignmentNotifyObserver()}, router.RunConfig{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := s.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	waitRouterDeliveryState(t, s, disabled.Event.Seq, "assignment", "stopped")
	current, err := readTask(ctx, s.db, "presence", first.ID)
	if err != nil || current.OwnerOrigin != nil {
		t.Fatalf("disabled plugin assigned %+v %v", current, err)
	}
	if onlyPresence(t, s).State != "idle" {
		t.Fatal("disabling assignment also stopped presence")
	}
	if _, err := s.db.Exec(`UPDATE settings SET value='true' WHERE key='plugin.assignment.enabled'`); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.EmitRouterEvent(ctx, runtime.Registry(), router.EmitRequest{ID: "clock.tick", Version: 1, Source: "clock", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitRouterDeliveryState(t, s, enabled.Event.Seq, "assignment", "completed")
	stop()
	current, err = readTask(ctx, s.db, "presence", first.ID)
	if err != nil || current.OwnerOrigin == nil || *current.OwnerOrigin != agent.Origin {
		t.Fatalf("enabled plugin failed to allocate %+v %v", current, err)
	}
}

func TestAssignmentPluginReservesFromTaskEventBeforeTick(t *testing.T) {
	s, agent, first, second := assignmentPluginFixture(t)
	ctx := context.Background()
	if err := s.ReconcilePresence(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	definitions := append(append(nativePresenceDefinitions(), RouterTaskEventDefinitions()...), plugins.NotificationEventDefinitions()...)
	runtime, err := router.NewRuntime(s, s.EventBus(), definitions, []router.Plugin{
		plugins.PresencePlugin(s), plugins.AssignmentPlugin(s), assignmentNotifyObserver(),
	}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	defer stop()
	if _, err := s.DrainRouterTaskOutbox(ctx, runtime.Registry(), 100); err != nil {
		t.Fatal(err)
	}
	created := awaitSourceEvent(t, s, "task.created")
	waitRouterDeliveryState(t, s, created.Seq, "assignment", "completed")
	got, err := s.Task(ctx, first.ProjectIdent, first.ID)
	if err != nil || got.Task.Status != "todo" || got.Task.OwnerOrigin == nil || *got.Task.OwnerOrigin != agent.Origin {
		t.Fatalf("task event did not reserve oldest eligible work: %+v %v", got, err)
	}
	if err := s.ReconcileRouterAssignmentsWithRegistry(ctx, runtime.Registry(), created); err != nil {
		t.Fatal(err)
	}
	got, err = s.Task(ctx, second.ProjectIdent, second.ID)
	if err != nil || got.Task.OwnerOrigin != nil {
		t.Fatalf("repeated task event double-assigned the worker: %+v %v", got, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM assignment_offers", int64(1))
}
