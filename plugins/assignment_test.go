package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

type assignmentStoreFunc struct {
	enabled   func(context.Context, string) (bool, error)
	reconcile func(context.Context, *router.Registry, router.Event) error
	feedback  func(context.Context, router.Event) error
}

func (s assignmentStoreFunc) PluginEnabled(ctx context.Context, name string) (bool, error) {
	return s.enabled(ctx, name)
}

func (s assignmentStoreFunc) ReconcileRouterAssignmentsWithRegistry(ctx context.Context, registry *router.Registry, event router.Event) error {
	return s.reconcile(ctx, registry, event)
}
func (s assignmentStoreFunc) ReconcileRouterAssignmentFeedback(ctx context.Context, event router.Event) error {
	if s.feedback != nil {
		return s.feedback(ctx, event)
	}
	return nil
}

type assignmentRuntimeJournal struct {
	mu       sync.Mutex
	event    router.Event
	plugins  []string
	claimed  bool
	finished chan assignmentFinish
}

type assignmentFinish struct {
	plugin  string
	outcome router.Outcome
}

func (j *assignmentRuntimeJournal) EmitRouterEvent(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error) {
	return router.EmitResult{}, nil
}

func (j *assignmentRuntimeJournal) AcquireRouterRuntime(context.Context, *router.Registry, int64) (string, error) {
	return "assignment-test", nil
}

func (j *assignmentRuntimeJournal) ClaimRouterDeliveries(_ context.Context, _ string, _ *router.Registry, _ int64, capacity int) (router.ClaimBatch, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.claimed {
		return router.ClaimBatch{}, nil
	}
	j.claimed = true
	plugins := append([]string(nil), j.plugins...)
	if len(plugins) > capacity {
		plugins = plugins[:capacity]
	}
	runs := make([]router.ClaimedRun, len(plugins))
	for i, plugin := range plugins {
		runs[i] = router.ClaimedRun{Event: j.event, Plugin: plugin}
	}
	return router.ClaimBatch{Runs: runs}, nil
}

func (j *assignmentRuntimeJournal) FinishRouterDelivery(_ context.Context, _ string, _ *router.Registry, run router.ClaimedRun, outcome router.Outcome, _ int64) error {
	j.finished <- assignmentFinish{plugin: run.Plugin, outcome: outcome}
	return nil
}

func (j *assignmentRuntimeJournal) ReleaseRouterRuntime(context.Context, string) error { return nil }

func TestAssignmentPluginMetadataAndMissingPresenceDependency(t *testing.T) {
	plugin := AssignmentPlugin(assignmentStoreFunc{
		enabled:   func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(context.Context, *router.Registry, router.Event) error { return nil },
	})
	if plugin.Definition.ID != assignmentPluginID || plugin.Definition.FIFO || len(plugin.Definition.Subscriptions) != 5 {
		t.Fatalf("unexpected plugin definition: %+v", plugin.Definition)
	}
	sub := plugin.Definition.Subscriptions[0]
	if sub.Event != "clock.tick" || sub.MinVersion != 1 || sub.MaxVersion != 1 || !reflect.DeepEqual(sub.DependsOn, []string{"presence"}) {
		t.Fatalf("unexpected subscription: %+v", sub)
	}
	if len(plugin.PreExec) != 1 || len(plugin.Exec) != 1 || len(plugin.PostExec) != 0 {
		t.Fatalf("unexpected plugin phases: pre=%d exec=%d post=%d", len(plugin.PreExec), len(plugin.Exec), len(plugin.PostExec))
	}

	presence := router.Plugin{Definition: router.PluginDefinition{ID: presencePluginID, Subscriptions: []router.Subscription{{Event: "clock.tick", MinVersion: 1, MaxVersion: 1}}}}
	_, err := router.NewRuntime(&assignmentRuntimeJournal{}, eventbus.New(), assignmentTestDefinitions(), []router.Plugin{presence, plugin}, router.RunConfig{})
	if err != nil {
		t.Fatalf("assignment registration failed: %v", err)
	}
	_, err = router.NewRuntime(&assignmentRuntimeJournal{}, eventbus.New(), assignmentTestDefinitions(), []router.Plugin{plugin}, router.RunConfig{})
	if err == nil || !strings.Contains(err.Error(), `depends on "presence"`) {
		t.Fatalf("runtime without presence registration error = %v", err)
	}
}

func TestDecodeAssignmentFeedbackUsesNotifyResultContract(t *testing.T) {
	got, err := DecodeAssignmentFeedback(json.RawMessage(`{"target":"","status":"skipped","extra":1}`))
	if err != nil || got.Target != "" || got.Status != "skipped" {
		t.Fatalf("empty target contract: %+v %v", got, err)
	}
	for _, payload := range []string{`{"target":null,"status":"failed"}`, `{"target":"s","status":"failed"} {}`, `[]`} {
		if _, err := DecodeAssignmentFeedback(json.RawMessage(payload)); err == nil {
			t.Errorf("invalid notify.result accepted: %s", payload)
		}
	}
}

func TestAssignmentPluginExecPassesEventAndMapsStoreOutcomes(t *testing.T) {
	cause := router.Event{Seq: 41, UID: "tick-cause", ID: "clock.tick", Version: 1, Payload: []byte(`{"at":1723456789012}`), Source: "clock", EmittedAt: 1723456789012}
	var gotEvent router.Event
	store := assignmentStoreFunc{
		enabled: func(_ context.Context, plugin string) (bool, error) {
			if plugin != assignmentPluginID {
				t.Errorf("PluginEnabled name=%q, want %q", plugin, assignmentPluginID)
			}
			return true, nil
		},
		reconcile: func(_ context.Context, _ *router.Registry, event router.Event) error { gotEvent = event; return nil },
	}
	journal := &assignmentRuntimeJournal{event: cause, plugins: []string{assignmentPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := assignmentTestRuntime(t, journal, AssignmentPlugin(store))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	got := awaitAssignmentFinish(t, journal.finished, assignmentPluginID)
	if got.outcome.Kind != router.Continue {
		t.Fatalf("outcome=%+v, want Continue", got.outcome)
	}
	cancelAssignmentRuntime(t, cancel, done)
	if !reflect.DeepEqual(gotEvent, cause) {
		t.Fatalf("assignment event=%+v, want unchanged event %+v", gotEvent, cause)
	}
}

func TestAssignmentPluginDisabledStopsBeforeExec(t *testing.T) {
	var execCalls int
	store := assignmentStoreFunc{
		enabled:   func(context.Context, string) (bool, error) { return false, nil },
		reconcile: func(context.Context, *router.Registry, router.Event) error { execCalls++; return nil },
	}
	journal := &assignmentRuntimeJournal{event: router.Event{ID: "clock.tick", Version: 1}, plugins: []string{assignmentPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := assignmentTestRuntime(t, journal, AssignmentPlugin(store))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	got := awaitAssignmentFinish(t, journal.finished, assignmentPluginID)
	if got.outcome.Kind != router.Stop || execCalls != 0 {
		t.Fatalf("outcome=%+v exec calls=%d, want Stop and 0", got.outcome, execCalls)
	}
	cancelAssignmentRuntime(t, cancel, done)
}

func TestAssignmentPluginNilStoreAndStoreErrorsFail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store AssignmentStore
		want  string
	}{
		{name: "nil store", want: "assignment plugin store is nil"},
		{name: "settings error", store: assignmentStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, errors.New("settings unavailable") }}, want: "settings unavailable"},
		{name: "reconcile error", store: assignmentStoreFunc{enabled: func(context.Context, string) (bool, error) { return true, nil }, reconcile: func(context.Context, *router.Registry, router.Event) error { return errors.New("allocation failed") }}, want: "allocation failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := &assignmentRuntimeJournal{event: router.Event{ID: "clock.tick", Version: 1}, plugins: []string{assignmentPluginID}, finished: make(chan assignmentFinish, 1)}
			runtime := assignmentTestRuntime(t, journal, AssignmentPlugin(tc.store))
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runtime.Run(ctx) }()
			got := awaitAssignmentFinish(t, journal.finished, assignmentPluginID)
			if got.outcome.Kind != router.Fail || got.outcome.Error != tc.want {
				t.Fatalf("outcome=%+v, want Fail %q", got.outcome, tc.want)
			}
			cancelAssignmentRuntime(t, cancel, done)
		})
	}
}

func TestAssignmentPluginHonorsCancellationDuringPreExec(t *testing.T) {
	entered := make(chan struct{})
	store := assignmentStoreFunc{
		enabled: func(ctx context.Context, _ string) (bool, error) {
			close(entered)
			<-ctx.Done()
			return false, ctx.Err()
		},
		reconcile: func(context.Context, *router.Registry, router.Event) error {
			t.Error("reconcile called after cancellation")
			return nil
		},
	}
	journal := &assignmentRuntimeJournal{event: router.Event{ID: "clock.tick", Version: 1}, plugins: []string{assignmentPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime := assignmentTestRuntime(t, journal, AssignmentPlugin(store))
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

func assignmentTestRuntime(t *testing.T, journal router.Journal, assignment router.Plugin) *router.Runtime {
	t.Helper()
	presence := router.Plugin{Definition: router.PluginDefinition{ID: presencePluginID, Subscriptions: []router.Subscription{{Event: "clock.tick", MinVersion: 1, MaxVersion: 1}}}}
	runtime, err := router.NewRuntime(journal, eventbus.New(), assignmentTestDefinitions(), []router.Plugin{presence, assignment}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func awaitAssignmentFinish(t *testing.T, finished <-chan assignmentFinish, plugin string) assignmentFinish {
	t.Helper()
	select {
	case got := <-finished:
		if got.plugin != plugin {
			t.Fatalf("finished plugin=%q, want %q", got.plugin, plugin)
		}
		return got
	case <-time.After(time.Second):
		t.Fatal("assignment plugin did not finish")
		return assignmentFinish{}
	}
}

func cancelAssignmentRuntime(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime returned on cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not stop")
	}
}

func assignmentTestDefinitions() []router.EventDefinition {
	definitions := []router.EventDefinition{ClockDefinition()}
	for _, id := range []string{"agent.idle", "task.created", "task.unclaimed"} {
		definitions = append(definitions, router.EventDefinition{ID: id, Version: 1, Validate: router.Object})
	}
	return append(definitions, NotificationEventDefinitions()...)
}
