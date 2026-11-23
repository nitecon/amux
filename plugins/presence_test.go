package plugins

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

type presenceStoreFunc struct {
	reconcile func(context.Context, *router.Registry, router.Event) error
	drain     func(context.Context, *router.Registry, int) (int, error)
}

func (s presenceStoreFunc) ReconcileRouterPresence(ctx context.Context, registry *router.Registry, event router.Event) error {
	return s.reconcile(ctx, registry, event)
}

func (s presenceStoreFunc) DrainRouterPresenceOutbox(ctx context.Context, registry *router.Registry, limit int) (int, error) {
	return s.drain(ctx, registry, limit)
}

type presenceRuntimeJournal struct {
	mu       sync.Mutex
	registry *router.Registry
	event    router.Event
	claimed  bool
	finished chan router.Outcome
}

func (j *presenceRuntimeJournal) EmitRouterEvent(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error) {
	return router.EmitResult{}, nil
}

func (j *presenceRuntimeJournal) AcquireRouterRuntime(_ context.Context, registry *router.Registry, _ int64) (string, error) {
	j.registry = registry
	return "presence-test", nil
}

func (j *presenceRuntimeJournal) ClaimRouterDeliveries(_ context.Context, _ string, registry *router.Registry, _ int64, _ int) (router.ClaimBatch, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.claimed {
		return router.ClaimBatch{}, nil
	}
	j.claimed = true
	j.registry = registry
	return router.ClaimBatch{Runs: []router.ClaimedRun{{Event: j.event, Plugin: presencePluginID}}}, nil
}

func (j *presenceRuntimeJournal) FinishRouterDelivery(_ context.Context, _ string, registry *router.Registry, _ router.ClaimedRun, outcome router.Outcome, _ int64) error {
	j.registry = registry
	j.finished <- outcome
	return nil
}

func (j *presenceRuntimeJournal) ReleaseRouterRuntime(context.Context, string) error { return nil }

func TestPresencePluginMetadataAndExecForwardsRegistryAndCause(t *testing.T) {
	plugin := PresencePlugin(presenceStoreFunc{
		reconcile: func(context.Context, *router.Registry, router.Event) error { return nil },
		drain:     func(context.Context, *router.Registry, int) (int, error) { return 0, nil },
	})
	if plugin.Definition.ID != presencePluginID || plugin.Definition.FIFO || len(plugin.Definition.Subscriptions) != 4 {
		t.Fatalf("unexpected plugin definition: %+v", plugin.Definition)
	}
	sub := plugin.Definition.Subscriptions[0]
	if sub.Event != "clock.tick" || sub.MinVersion != 1 || sub.MaxVersion != 1 || len(sub.DependsOn) != 0 {
		t.Fatalf("unexpected subscription: %+v", sub)
	}
	heartbeatSub := plugin.Definition.Subscriptions[1]
	if heartbeatSub.Event != ClientHeartbeatID || heartbeatSub.MinVersion != 1 || heartbeatSub.MaxVersion != 1 || len(heartbeatSub.DependsOn) != 0 {
		t.Fatalf("unexpected heartbeat subscription: %+v", heartbeatSub)
	}

	cause := router.Event{Seq: 41, UID: "tick-cause", ID: "clock.tick", Version: 1, Payload: []byte(`{"at":1723456789012}`), Source: "clock", EmittedAt: 1723456789012}
	var gotRegistry *router.Registry
	var gotEvent router.Event
	plugin = PresencePlugin(presenceStoreFunc{
		reconcile: func(_ context.Context, registry *router.Registry, event router.Event) error {
			gotRegistry, gotEvent = registry, event
			return nil
		},
		drain: func(context.Context, *router.Registry, int) (int, error) { return 0, nil },
	})
	journal := &presenceRuntimeJournal{event: cause, finished: make(chan router.Outcome, 1)}
	bus := eventbus.New()
	runtime, err := router.NewRuntime(journal, bus, presenceTestDefinitions(), []router.Plugin{plugin}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case outcome := <-journal.finished:
		if outcome.Kind != router.Continue {
			t.Fatalf("plugin outcome: %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("presence plugin did not execute")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime returned on cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not stop")
	}
	if gotRegistry == nil || gotRegistry != journal.registry || gotRegistry != runtime.Registry() {
		t.Fatal("presence reconciliation did not receive the immutable runtime registry")
	}
	if gotEvent.Seq != cause.Seq || gotEvent.UID != cause.UID || gotEvent.ID != cause.ID || gotEvent.Version != cause.Version || gotEvent.Source != cause.Source || string(gotEvent.Payload) != string(cause.Payload) {
		t.Fatalf("presence reconciliation cause=%+v, want %+v", gotEvent, cause)
	}
}

func TestPresencePluginNilStoreAndStoreErrorFail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event router.Event
		store PresenceStore
		want  string
	}{
		{name: "nil store on heartbeat", event: router.Event{ID: ClientHeartbeatID, Version: 1}, want: "presence plugin store is nil"},
		{name: "store error on clock", event: router.Event{ID: "clock.tick", Version: 1}, store: presenceStoreFunc{
			reconcile: func(context.Context, *router.Registry, router.Event) error { return errors.New("reconcile failed") },
			drain:     func(context.Context, *router.Registry, int) (int, error) { return 0, nil },
		}, want: "reconcile failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := &presenceRuntimeJournal{event: tc.event, finished: make(chan router.Outcome, 1)}
			runtime, err := router.NewRuntime(journal, eventbus.New(), presenceTestDefinitions(), []router.Plugin{PresencePlugin(tc.store)}, router.RunConfig{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runtime.Run(ctx) }()
			select {
			case outcome := <-journal.finished:
				if outcome.Kind != router.Fail || outcome.Error != tc.want {
					t.Fatalf("outcome=%+v, want Fail %q", outcome, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("plugin did not execute")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("runtime returned on cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("runtime did not stop")
			}
		})
	}
}

func TestPresenceHeartbeatForwardsToStorageCallback(t *testing.T) {
	cause := router.Event{Seq: 52, UID: "accepted-heartbeat", ID: ClientHeartbeatID, Version: 1, Payload: []byte(`{"agents":[]}`), Source: "client:test", EmittedAt: 900}
	var gotRegistry *router.Registry
	var gotEvent router.Event
	store := presenceStoreFunc{
		reconcile: func(_ context.Context, registry *router.Registry, event router.Event) error {
			gotRegistry, gotEvent = registry, event
			return nil
		},
		drain: func(context.Context, *router.Registry, int) (int, error) { return 0, nil },
	}
	journal := &presenceRuntimeJournal{event: cause, finished: make(chan router.Outcome, 1)}
	runtime, err := router.NewRuntime(journal, eventbus.New(), presenceTestDefinitions(), []router.Plugin{PresencePlugin(store)}, router.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case outcome := <-journal.finished:
		if outcome.Kind != router.Continue {
			t.Fatalf("presence heartbeat outcome=%+v, want Continue", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("presence did not complete heartbeat event")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runtime returned on cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not stop")
	}
	if gotRegistry != runtime.Registry() || gotRegistry != journal.registry {
		t.Fatalf("heartbeat callback registry=%p, runtime=%p, journal=%p", gotRegistry, runtime.Registry(), journal.registry)
	}
	if gotEvent.Seq != cause.Seq || gotEvent.UID != cause.UID || gotEvent.ID != cause.ID || gotEvent.Version != cause.Version || gotEvent.Source != cause.Source || gotEvent.EmittedAt != cause.EmittedAt || string(gotEvent.Payload) != string(cause.Payload) {
		t.Fatalf("heartbeat callback event=%+v, want %+v", gotEvent, cause)
	}
}

func TestPresenceSourceStartupWakeRetryCancelAndOverflow(t *testing.T) {
	t.Run("startup and relevant wakes", func(t *testing.T) {
		bus := eventbus.New()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := make(chan int, 8)
		var mu sync.Mutex
		count := 0
		done := startPresenceSource(ctx, presenceStoreFunc{
			reconcile: func(context.Context, *router.Registry, router.Event) error { return nil },
			drain: func(_ context.Context, registry *router.Registry, limit int) (int, error) {
				if registry == nil || limit != 200 {
					t.Errorf("registry=%p limit=%d", registry, limit)
				}
				mu.Lock()
				count++
				current := count
				mu.Unlock()
				calls <- current
				return 0, nil
			},
		}, sourceRegistry(t), bus)
		if got := <-calls; got != 1 {
			t.Fatalf("startup drain count=%d", got)
		}
		if err := bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged}); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-calls:
			t.Fatalf("RouterChanged caused drain %d", got)
		case <-time.After(30 * time.Millisecond):
		}
		for _, topic := range []eventbus.Topic{eventbus.PresenceChanged, eventbus.Tick} {
			if err := bus.Publish(eventbus.Event{Topic: topic}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-calls:
			case <-time.After(time.Second):
				t.Fatalf("%s did not wake the source", topic)
			}
		}
		stopSource(t, cancel, done)
	})

	t.Run("bounded batches", func(t *testing.T) {
		bus := eventbus.New()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := make(chan int, 4)
		remaining := 2
		done := startPresenceSource(ctx, presenceStoreFunc{
			reconcile: func(context.Context, *router.Registry, router.Event) error { return nil },
			drain: func(_ context.Context, _ *router.Registry, limit int) (int, error) {
				calls <- limit
				remaining--
				if remaining > 0 {
					return limit, nil
				}
				return 0, nil
			},
		}, sourceRegistry(t), bus)
		for i := 0; i < 2; i++ {
			if got := <-calls; got != 200 {
				t.Fatalf("drain limit=%d, want 200", got)
			}
		}
		stopSource(t, cancel, done)
	})

	t.Run("retry wake and cancellation", func(t *testing.T) {
		bus := eventbus.New()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := make(chan struct{}, 4)
		drainCalls := 0
		done := startPresenceSource(ctx, presenceStoreFunc{
			reconcile: func(context.Context, *router.Registry, router.Event) error { return nil },
			drain: func(context.Context, *router.Registry, int) (int, error) {
				drainCalls++
				calls <- struct{}{}
				if drainCalls == 1 {
					return 0, errors.New("transient presence outbox failure")
				}
				return 0, nil
			},
		}, sourceRegistry(t), bus)
		<-calls
		if err := bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("PresenceChanged did not accelerate retry")
		}
		stopSource(t, cancel, done)
	})

	t.Run("overflow resubscribes and reconciles", func(t *testing.T) {
		bus := eventbus.New()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		calls := 0
		done := startPresenceSource(ctx, presenceStoreFunc{
			reconcile: func(context.Context, *router.Registry, router.Event) error { return nil },
			drain: func(callCtx context.Context, _ *router.Registry, _ int) (int, error) {
				calls++
				if calls == 1 {
					close(entered)
					select {
					case <-release:
					case <-callCtx.Done():
					}
				}
				return 0, nil
			},
		}, sourceRegistry(t), bus)
		<-entered
		for i := 0; i < sourceSubscriptionBuffer+1; i++ {
			if err := bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged}); err != nil {
				t.Fatal(err)
			}
		}
		bus.Close()
		close(release)
		select {
		case err := <-done:
			if !errors.Is(err, eventbus.ErrBusClosed) {
				t.Fatalf("source error=%v, want ErrBusClosed", err)
			}
		case <-time.After(time.Second):
			t.Fatal("source did not observe bus closure after overflow")
		}
		if calls < 2 {
			t.Fatalf("overflow did not force reconciliation, calls=%d", calls)
		}
		cancel()
	})
}

func startPresenceSource(ctx context.Context, store PresenceStore, registry *router.Registry, bus *eventbus.Bus) <-chan error {
	ready := bus.Subscribe(1)
	ready.Unsubscribe()
	done := make(chan error, 1)
	go func() { done <- RunPresenceSource(ctx, store, registry, bus) }()
	return done
}

func presenceTestDefinitions() []router.EventDefinition {
	return append([]router.EventDefinition{ClockDefinition(), ClientHeartbeatDefinition()}, ActivityEventDefinitions()...)
}
