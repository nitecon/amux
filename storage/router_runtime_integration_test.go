package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

func waitRouterDeliveryState(t *testing.T, s *Store, seq int64, plugin, state string) {
	t.Helper()
	sub := s.EventBus().Subscribe(64)
	defer sub.Unsubscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		rows, err := s.RouterDeliveries(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Plugin == plugin && row.State == state {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("delivery %d/%s never reached %s: %+v", seq, plugin, state, rows)
		case _, ok := <-sub.C:
			if !ok {
				t.Fatal("delivery watcher closed")
			}
		}
	}
}

func awaitRouterSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("router signal timed out")
	}
}

func runRouterFixture(t *testing.T, runtime *router.Runtime) context.CancelFunc {
	t.Helper()
	return runOwnedRouterFixture(t, runtime.Run)
}

func runOwnedRouterFixture(t *testing.T, run func(context.Context) error) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("runtime shutdown: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("runtime did not join owned work")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestRouterRuntimeUsesDurableDependenciesAndParallelChannels(t *testing.T) {
	s := testStore(t)
	gate, baseStarted, otherStarted, dependentStarted := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var order []string
	record := func(name string) router.Step {
		return func(context.Context, router.Event, *router.StepContext) router.Outcome {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return router.Outcome{Kind: router.Continue}
		}
	}
	subscription := func(deps ...string) []router.Subscription {
		return []router.Subscription{{Event: "test.runtime", MinVersion: 1, MaxVersion: 1, DependsOn: deps}}
	}
	runtime, err := router.NewRuntime(s, s.EventBus(), []router.EventDefinition{{ID: "test.runtime", Version: 1, Validate: router.Object}}, []router.Plugin{
		{Definition: router.PluginDefinition{ID: "base", Subscriptions: subscription()}, PreExec: []router.Step{record("pre")}, Exec: []router.Step{record("exec"), func(ctx context.Context, _ router.Event, _ *router.StepContext) router.Outcome {
			close(baseStarted)
			select {
			case <-gate:
			case <-ctx.Done():
			}
			return router.Outcome{Kind: router.Continue}
		}}, PostExec: []router.Step{record("post")}},
		{Definition: router.PluginDefinition{ID: "other", Subscriptions: subscription()}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			close(otherStarted)
			return router.Outcome{Kind: router.Continue}
		}}},
		{Definition: router.PluginDefinition{ID: "dependent", Subscriptions: subscription("base")}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			close(dependentStarted)
			return record("dependent")(context.Background(), router.Event{}, nil)
		}}},
	}, router.RunConfig{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), router.EmitRequest{ID: "test.runtime", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	awaitRouterSignal(t, baseStarted)
	awaitRouterSignal(t, otherStarted)
	select {
	case <-dependentStarted:
		t.Fatal("dependent ran before base committed completion")
	default:
	}
	close(gate)
	awaitRouterSignal(t, dependentStarted)
	waitRouterDeliveryState(t, s, result.Event.Seq, "dependent", "completed")
	stop()
	mu.Lock()
	defer mu.Unlock()
	want := []string{"pre", "exec", "post", "dependent"}
	if len(order) != len(want) {
		t.Fatalf("phase order %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("phase order %v", order)
		}
	}
}

func TestRouterRuntimeTickMakesDurableRetryEligible(t *testing.T) {
	for _, panicFirst := range []bool{false, true} {
		name := "failure"
		if panicFirst {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			var clock atomic.Int64
			var runs, preRuns, dependentRuns atomic.Int32
			second := make(chan struct{})
			runtime, err := router.NewRuntime(s, s.EventBus(), []router.EventDefinition{{ID: "test.retry", Version: 1, Validate: router.Object}}, []router.Plugin{
				{Definition: router.PluginDefinition{ID: "retry", Subscriptions: []router.Subscription{{Event: "test.retry", MinVersion: 1, MaxVersion: 1}}}, PreExec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
					preRuns.Add(1)
					return router.Outcome{Kind: router.Continue}
				}}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
					attempt := runs.Add(1)
					if attempt == 1 {
						if panicFirst {
							panic("offline")
						}
						return router.Outcome{Kind: router.Fail, Error: "offline"}
					}
					if attempt == 2 {
						close(second)
					}
					return router.Outcome{Kind: router.Continue}
				}}},
				{Definition: router.PluginDefinition{ID: "dependent", Subscriptions: []router.Subscription{{Event: "test.retry", MinVersion: 1, MaxVersion: 1, DependsOn: []string{"retry"}}}}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
					dependentRuns.Add(1)
					return router.Outcome{Kind: router.Continue}
				}}},
			}, router.RunConfig{Capacity: 1, Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
			if err != nil {
				t.Fatal(err)
			}
			event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), router.EmitRequest{ID: "test.retry", Version: 1, Source: "test", Payload: []byte(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			clock.Store(event.Event.EmittedAt)
			stop := runRouterFixture(t, runtime)
			waitRouterDeliveryState(t, s, event.Event.Seq, "retry", "retrying")
			if runs.Load() != 1 || preRuns.Load() != 1 || dependentRuns.Load() != 0 {
				t.Fatalf("retry/dependency boundary runs=%d pre=%d dependent=%d", runs.Load(), preRuns.Load(), dependentRuns.Load())
			}
			clock.Add(1000)
			if err := s.EventBus().Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
				t.Fatal(err)
			}
			awaitRouterSignal(t, second)
			waitRouterDeliveryState(t, s, event.Event.Seq, "dependent", "completed")
			stop()
			if runs.Load() != 2 || preRuns.Load() != 2 || dependentRuns.Load() != 1 {
				t.Fatalf("retry did not restart first phase: runs=%d pre=%d dependent=%d", runs.Load(), preRuns.Load(), dependentRuns.Load())
			}
		})
	}
}

type flakyRouterFinish struct {
	router.Journal
	failed chan struct{}
	once   sync.Once
}

func (j *flakyRouterFinish) FinishRouterDelivery(ctx context.Context, token string, r *router.Registry, run router.ClaimedRun, outcome router.Outcome, now int64) error {
	fail := false
	j.once.Do(func() { fail = true; close(j.failed) })
	if fail {
		return errors.New("fixture completion write unavailable")
	}
	return j.Journal.FinishRouterDelivery(ctx, token, r, run, outcome, now)
}

func TestRouterRuntimeRetriesCompletionWriteWithoutRepeatingStep(t *testing.T) {
	s := testStore(t)
	journal := &flakyRouterFinish{Journal: s, failed: make(chan struct{})}
	var executions atomic.Int32
	runtime, err := router.NewRuntime(journal, s.EventBus(), []router.EventDefinition{{ID: "test.finish", Version: 1, Validate: router.Object}}, []router.Plugin{{Definition: router.PluginDefinition{ID: "finish", Subscriptions: []router.Subscription{{Event: "test.finish", MinVersion: 1, MaxVersion: 1}}}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
		executions.Add(1)
		return router.Outcome{Kind: router.Continue}
	}}}}, router.RunConfig{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), router.EmitRequest{ID: "test.finish", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	awaitRouterSignal(t, journal.failed)
	waitRouterDeliveryState(t, s, event.Event.Seq, "finish", "completed")
	stop()
	if executions.Load() != 1 {
		t.Fatalf("completion failure repeated side effects %d times", executions.Load())
	}
}

func TestRouterRuntimeStopSkipsOnlyDeclaredDependents(t *testing.T) {
	s := testStore(t)
	var dependentRuns, postRuns atomic.Int32
	sub := func(deps ...string) []router.Subscription {
		return []router.Subscription{{Event: "test.stop", MinVersion: 1, MaxVersion: 1, DependsOn: deps}}
	}
	runtime, err := router.NewRuntime(s, s.EventBus(), []router.EventDefinition{{ID: "test.stop", Version: 1, Validate: router.Object}}, []router.Plugin{
		{Definition: router.PluginDefinition{ID: "veto", Subscriptions: sub()}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			return router.Outcome{Kind: router.Stop}
		}}, PostExec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			postRuns.Add(1)
			return router.Outcome{Kind: router.Continue}
		}}},
		{Definition: router.PluginDefinition{ID: "dependent", Subscriptions: sub("veto")}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			dependentRuns.Add(1)
			return router.Outcome{Kind: router.Continue}
		}}},
		{Definition: router.PluginDefinition{ID: "independent", Subscriptions: sub()}, Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			return router.Outcome{Kind: router.Continue}
		}}},
	}, router.RunConfig{Capacity: 3})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), router.EmitRequest{ID: "test.stop", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	waitRouterDeliveryState(t, s, event.Event.Seq, "veto", "stopped")
	waitRouterDeliveryState(t, s, event.Event.Seq, "dependent", "skipped")
	waitRouterDeliveryState(t, s, event.Event.Seq, "independent", "completed")
	stop()
	if dependentRuns.Load() != 0 || postRuns.Load() != 0 {
		t.Fatalf("veto bypassed: dependent=%d post=%d", dependentRuns.Load(), postRuns.Load())
	}
}

func TestRouterRuntimeStepEmissionPersistsSourceAndCorrelation(t *testing.T) {
	s := testStore(t)
	observed := make(chan router.Event, 1)
	runtime, err := router.NewRuntime(s, s.EventBus(), []router.EventDefinition{{ID: "test.cause", Version: 1, Validate: router.Object}, {ID: "test.effect", Version: 1, Validate: router.Object}}, []router.Plugin{
		{Definition: router.PluginDefinition{ID: "producer", Subscriptions: []router.Subscription{{Event: "test.cause", MinVersion: 1, MaxVersion: 1}}}, Exec: []router.Step{func(ctx context.Context, _ router.Event, step *router.StepContext) router.Outcome {
			if _, err := step.Emit(ctx, "test.effect", 1, []byte(`{"effect":true}`)); err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}}},
		{Definition: router.PluginDefinition{ID: "observer", Subscriptions: []router.Subscription{{Event: "test.effect", MinVersion: 1, MaxVersion: 1}}}, Exec: []router.Step{func(ctx context.Context, event router.Event, _ *router.StepContext) router.Outcome {
			select {
			case observed <- event:
			case <-ctx.Done():
			}
			return router.Outcome{Kind: router.Continue}
		}}},
	}, router.RunConfig{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	cause, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), router.EmitRequest{ID: "test.cause", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	var effect router.Event
	select {
	case effect = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("correlated event was not delivered")
	}
	if effect.Source != "plugin:producer" || effect.Correlation == nil || *effect.Correlation != cause.Event.UID {
		t.Fatalf("emission provenance %+v", effect)
	}
	waitRouterDeliveryState(t, s, effect.Seq, "observer", "completed")
	stop()
}

func TestRouterRecoveryReplaysUnfinishedAndRetainsTerminalFailures(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "router-recovery.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defs := []router.EventDefinition{{ID: "test.recover", Version: 1, Validate: router.Object}}
	plugin := func(id string, limit uint32) router.PluginDefinition {
		return router.PluginDefinition{ID: id, MaxAttempts: limit, Subscriptions: []router.Subscription{{Event: "test.recover", MinVersion: 1, MaxVersion: 1}}}
	}
	original, err := router.NewRegistry(defs, []router.PluginDefinition{plugin("working", 0), plugin("removed", 0), plugin("exhausted", 1)})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(ctx, original, router.EmitRequest{ID: "test.recover", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.AcquireRouterRuntime(ctx, original, event.Event.EmittedAt)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.ClaimRouterDeliveries(ctx, token, original, event.Event.EmittedAt, 3)
	if err != nil || len(batch.Runs) != 3 {
		t.Fatalf("initial claims %+v %v", batch, err)
	}
	for _, run := range batch.Runs {
		if run.Plugin == "exhausted" {
			if err := s.FinishRouterDelivery(ctx, token, original, run, router.Outcome{Kind: router.Fail, Error: "terminal"}, event.Event.EmittedAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Closing without releasing models a crash after durable claim, before finish.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	current, err := router.NewRegistry(defs, []router.PluginDefinition{plugin("working", 0), plugin("exhausted", 1)})
	if err != nil {
		t.Fatal(err)
	}
	recoveredToken, err := reopened.AcquireRouterRuntime(ctx, current, event.Event.EmittedAt+1)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := reopened.RouterDeliveries(ctx, event.Event.Seq)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]router.Delivery{}
	for _, row := range rows {
		states[row.Plugin] = row
	}
	if states["working"].State != "pending" || states["removed"].State != "skipped" || states["exhausted"].State != "failed" || states["exhausted"].Attempts != 1 {
		t.Fatalf("recovered states %+v", states)
	}
	batch, err = reopened.ClaimRouterDeliveries(ctx, recoveredToken, current, event.Event.EmittedAt+1, 3)
	if err != nil || len(batch.Runs) != 1 || batch.Runs[0].Plugin != "working" || batch.Runs[0].Event.UID != event.Event.UID {
		t.Fatalf("replayed claim %+v %v", batch, err)
	}
	if err := reopened.FinishRouterDelivery(ctx, recoveredToken, current, batch.Runs[0], router.Outcome{Kind: router.Continue}, event.Event.EmittedAt+1); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ReleaseRouterRuntime(ctx, recoveredToken); err != nil {
		t.Fatal(err)
	}
}

func TestRouterRuntimeCancellationJoinsOwnedBlockedStep(t *testing.T) {
	s := testStore(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	runtime, err := router.NewRuntime(s, s.EventBus(), []router.EventDefinition{{ID: "test.shutdown", Version: 1, Validate: router.Object}}, []router.Plugin{{Definition: router.PluginDefinition{ID: "blocked", Subscriptions: []router.Subscription{{Event: "test.shutdown", MinVersion: 1, MaxVersion: 1}}}, Exec: []router.Step{func(ctx context.Context, _ router.Event, _ *router.StepContext) router.Outcome {
		close(started)
		<-ctx.Done()
		close(stopped)
		return router.Outcome{Kind: router.Continue}
	}}}}, router.RunConfig{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), router.EmitRequest{ID: "test.shutdown", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	awaitRouterSignal(t, started)
	stop()
	select {
	case <-stopped:
	default:
		t.Fatal("runtime returned before owned step joined")
	}
	rows, err := s.RouterDeliveries(context.Background(), event.Event.Seq)
	if err != nil || len(rows) != 1 || rows[0].State != "pending" {
		t.Fatalf("interrupted delivery not recoverable: %+v %v", rows, err)
	}
	token, err := s.AcquireRouterRuntime(context.Background(), runtime.Registry(), event.Event.EmittedAt+1)
	if err != nil {
		t.Fatalf("shutdown retained runtime admission: %v", err)
	}
	if err := s.ReleaseRouterRuntime(context.Background(), token); err != nil {
		t.Fatal(err)
	}
}
