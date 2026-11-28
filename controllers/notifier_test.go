package controllers

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

type notifierStoreFunc struct {
	enabled   func(context.Context, string) (bool, error)
	reconcile func(context.Context, *fism.Registry, fism.Event) error
}

func (s notifierStoreFunc) PluginEnabled(ctx context.Context, plugin string) (bool, error) {
	return s.enabled(ctx, plugin)
}

func (s notifierStoreFunc) ReconcileRouterNotifiers(ctx context.Context, registry *fism.Registry, event fism.Event) error {
	return s.reconcile(ctx, registry, event)
}

func TestNotifierPluginMetadata(t *testing.T) {
	plugin := NotifierController(notifierStoreFunc{})
	if plugin.Definition.ID != notifierPluginID || plugin.Definition.FIFO || plugin.Definition.MaxAttempts != 0 {
		t.Fatalf("unexpected plugin definition: %+v", plugin.Definition)
	}
	wantEvents := []string{"task.created", "task.commented", "task.user_review", "task.completed"}
	if len(plugin.Definition.Subscriptions) != len(wantEvents) {
		t.Fatalf("subscriptions=%+v, want %v", plugin.Definition.Subscriptions, wantEvents)
	}
	for i, sub := range plugin.Definition.Subscriptions {
		if sub.Event != wantEvents[i] || sub.MinVersion != 1 || sub.MaxVersion != 1 || len(sub.DependsOn) != 0 {
			t.Fatalf("subscription %d=%+v, want %s v1 without dependencies", i, sub, wantEvents[i])
		}
	}
	if len(plugin.PreExec) != 1 || len(plugin.Exec) != 1 || len(plugin.PostExec) != 0 {
		t.Fatalf("unexpected plugin phases: pre=%d exec=%d post=%d", len(plugin.PreExec), len(plugin.Exec), len(plugin.PostExec))
	}

	events := notifierTestEventDefinitions()
	_, err := fism.NewRuntime(&assignmentRuntimeJournal{}, eventbus.New(), events, []fism.Controller{plugin}, fism.RunConfig{})
	if err != nil {
		t.Fatalf("notifier runtime registration failed: %v", err)
	}
}

func TestNotifierPluginPreExecOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store NotifierStore
		want  fism.Outcome
	}{
		{name: "enabled", store: notifierStoreFunc{enabled: func(_ context.Context, plugin string) (bool, error) { return plugin == notifierPluginID, nil }}, want: fism.Outcome{Kind: fism.Continue}},
		{name: "disabled", store: notifierStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, nil }}, want: fism.Outcome{Kind: fism.Stop}},
		{name: "settings error", store: notifierStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, errors.New("settings unavailable") }}, want: fism.Outcome{Kind: fism.Fail, Error: "settings unavailable"}},
		{name: "nil store", want: fism.Outcome{Kind: fism.Fail, Error: "notifier plugin store is nil"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NotifierController(tc.store).PreExec[0](context.Background(), fism.Event{}, nil)
			if got != tc.want {
				t.Fatalf("outcome=%+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNotifierPluginDisabledSkipsExec(t *testing.T) {
	var reconcileCalls int
	store := notifierStoreFunc{
		enabled:   func(context.Context, string) (bool, error) { return false, nil },
		reconcile: func(context.Context, *fism.Registry, fism.Event) error { reconcileCalls++; return nil },
	}
	cause := fism.Event{Seq: 5, UID: "disabled", ID: "task.created", Version: 1, Payload: []byte(`{"task_id":"t-1"}`)}
	journal := &assignmentRuntimeJournal{event: cause, plugins: []string{notifierPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := notifierTestRuntime(t, journal, NotifierController(store))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	got := awaitAssignmentFinish(t, journal.finished, notifierPluginID)
	if got.outcome.Kind != fism.Stop || reconcileCalls != 0 {
		t.Fatalf("outcome=%+v reconcile calls=%d, want Stop and 0", got.outcome, reconcileCalls)
	}
	cancelAssignmentRuntime(t, cancel, done)
}

func TestNotifierPluginExecForwardsEventAndRegistry(t *testing.T) {
	cause := fism.Event{Seq: 41, UID: "task-cause", ID: "task.commented", Version: 1, Payload: []byte(`{"task_id":"t-9","comment_id":"c-3"}`), Source: "tasks", EmittedAt: 1723456789012}
	var gotEvent fism.Event
	var gotRegistry *fism.Registry
	store := notifierStoreFunc{
		enabled: func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(_ context.Context, registry *fism.Registry, event fism.Event) error {
			gotRegistry, gotEvent = registry, event
			return nil
		},
	}
	journal := &assignmentRuntimeJournal{event: cause, plugins: []string{notifierPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := notifierTestRuntime(t, journal, NotifierController(store))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	got := awaitAssignmentFinish(t, journal.finished, notifierPluginID)
	if got.outcome.Kind != fism.Continue {
		t.Fatalf("outcome=%+v, want Continue", got.outcome)
	}
	cancelAssignmentRuntime(t, cancel, done)
	if gotRegistry != runtime.Registry() || !reflect.DeepEqual(gotEvent, cause) {
		t.Fatalf("reconcile registry=%p event=%+v, want registry=%p event=%+v", gotRegistry, gotEvent, runtime.Registry(), cause)
	}
}

func TestNotifierPluginExecErrorsFail(t *testing.T) {
	plugin := NotifierController(notifierStoreFunc{
		enabled: func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(context.Context, *fism.Registry, fism.Event) error {
			return errors.New("notifier reconciliation failed")
		},
	})
	if got := plugin.Exec[0](context.Background(), fism.Event{}, nil); got != (fism.Outcome{Kind: fism.Fail, Error: "notifier plugin registry is nil"}) {
		t.Fatalf("nil step context outcome=%+v", got)
	}
	if got := NotifierController(nil).Exec[0](context.Background(), fism.Event{}, nil); got != (fism.Outcome{Kind: fism.Fail, Error: "notifier plugin store is nil"}) {
		t.Fatalf("nil store outcome=%+v", got)
	}
	cause := fism.Event{ID: "task.completed", Version: 1, Payload: []byte(`{"task_id":"t-1"}`)}
	journal := &assignmentRuntimeJournal{event: cause, plugins: []string{notifierPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := notifierTestRuntime(t, journal, plugin)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	got := awaitAssignmentFinish(t, journal.finished, notifierPluginID)
	if got.outcome.Kind != fism.Fail || got.outcome.Error != "notifier reconciliation failed" {
		t.Fatalf("outcome=%+v, want reconciliation failure", got.outcome)
	}
	cancelAssignmentRuntime(t, cancel, done)
}

func TestNotifierPluginHonorsCancellationDuringPreExec(t *testing.T) {
	entered := make(chan struct{})
	store := notifierStoreFunc{
		enabled: func(ctx context.Context, _ string) (bool, error) {
			close(entered)
			<-ctx.Done()
			return false, ctx.Err()
		},
		reconcile: func(context.Context, *fism.Registry, fism.Event) error {
			t.Error("reconcile called after cancellation")
			return nil
		},
	}
	journal := &assignmentRuntimeJournal{event: fism.Event{ID: "task.created", Version: 1, Payload: []byte(`{}`)}, plugins: []string{notifierPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := notifierTestRuntime(t, journal, NotifierController(store))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("PluginEnabled was not called")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime returned on cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not join canceled pre-execution")
	}
}

func notifierTestRuntime(t *testing.T, journal fism.Journal, plugin fism.Controller) *fism.Runtime {
	t.Helper()
	runtime, err := fism.NewRuntime(journal, eventbus.New(), notifierTestEventDefinitions(), []fism.Controller{plugin}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func notifierTestEventDefinitions() []fism.EventDefinition {
	return []fism.EventDefinition{
		{ID: "task.created", Version: 1, Validate: fism.Object},
		{ID: "task.commented", Version: 1, Validate: fism.Object},
		{ID: "task.user_review", Version: 1, Validate: fism.Object},
		{ID: "task.completed", Version: 1, Validate: fism.Object},
	}
}
