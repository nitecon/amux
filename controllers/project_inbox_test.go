package controllers

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

type projectInboxStoreFunc func(context.Context, fism.Event) error

func (f projectInboxStoreFunc) ReconcileProjectInbox(ctx context.Context, event fism.Event) error {
	return f(ctx, event)
}

func TestProjectInboxPluginDependsOnPresenceForSharedClock(t *testing.T) {
	plugin := ProjectInboxController(projectInboxStoreFunc(func(context.Context, fism.Event) error { return nil }))
	if plugin.Definition.ID != projectInboxPluginID || plugin.Definition.FIFO || len(plugin.Definition.Subscriptions) != 1 {
		t.Fatalf("unexpected project inbox plugin definition: %+v", plugin.Definition)
	}
	sub := plugin.Definition.Subscriptions[0]
	if sub.Event != "clock.tick" || sub.MinVersion != 1 || sub.MaxVersion != 1 || !reflect.DeepEqual(sub.DependsOn, []string{presencePluginID}) {
		t.Fatalf("unexpected project inbox subscription: %+v", sub)
	}
	if len(plugin.PreExec) != 0 || len(plugin.Exec) != 1 || len(plugin.PostExec) != 0 {
		t.Fatalf("unexpected plugin phases: pre=%d exec=%d post=%d", len(plugin.PreExec), len(plugin.Exec), len(plugin.PostExec))
	}

	presence := fism.Controller{Definition: fism.ControllerDefinition{
		ID:            presencePluginID,
		Subscriptions: []fism.Subscription{{Event: "clock.tick", MinVersion: 1, MaxVersion: 1}},
	}}
	events := []fism.EventDefinition{ClockDefinition()}
	if _, err := fism.NewRuntime(&assignmentRuntimeJournal{}, eventbus.New(), events, []fism.Controller{presence, plugin}, fism.RunConfig{}); err != nil {
		t.Fatalf("register with presence: %v", err)
	}
	if _, err := fism.NewRuntime(&assignmentRuntimeJournal{}, eventbus.New(), events, []fism.Controller{plugin}, fism.RunConfig{}); err == nil {
		t.Fatal("registered without the required presence dependency")
	}
}

func TestProjectInboxPluginProcessesTickAndJoinsOnShutdown(t *testing.T) {
	cause := fism.Event{Seq: 8, UID: "tick-8", ID: "clock.tick", Version: 1, Payload: []byte(`{"at":1234}`), Source: "clock", EmittedAt: 1234}
	var got fism.Event
	plugin := ProjectInboxController(projectInboxStoreFunc(func(_ context.Context, event fism.Event) error {
		got = event
		return nil
	}))
	journal := &assignmentRuntimeJournal{event: cause, plugins: []string{projectInboxPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := assignmentTestRuntime(t, journal, plugin)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	finished := awaitAssignmentFinish(t, journal.finished, projectInboxPluginID)
	if finished.outcome.Kind != fism.Continue {
		t.Fatalf("tick outcome=%+v, want Continue", finished.outcome)
	}
	if !reflect.DeepEqual(got, cause) {
		t.Fatalf("storage received event %+v, want %+v", got, cause)
	}
	cancelAssignmentRuntime(t, cancel, done)
}

func TestProjectInboxPluginRetriesStoreFailureAndRejectsNilStore(t *testing.T) {
	event := fism.Event{ID: "clock.tick", Version: 1}
	wantErr := errors.New("inbox reconciliation failed")
	failed := ProjectInboxController(projectInboxStoreFunc(func(context.Context, fism.Event) error { return wantErr }))
	if got := failed.Exec[0](context.Background(), event, nil); got.Kind != fism.Fail || got.Error != wantErr.Error() {
		t.Fatalf("store failure outcome=%+v, want Fail %q", got, wantErr)
	}
	if got := ProjectInboxController(nil).Exec[0](context.Background(), event, nil); got.Kind != fism.Fail || got.Error != "project inbox plugin store is nil" {
		t.Fatalf("nil store outcome=%+v", got)
	}
}
