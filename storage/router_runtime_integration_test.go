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
	"github.com/nitecon/amux/fism"
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

func runRouterFixture(t *testing.T, runtime *fism.Runtime) context.CancelFunc {
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
	record := func(name string) fism.Step {
		return func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return fism.Outcome{Kind: fism.Continue}
		}
	}
	subscription := func(deps ...string) []fism.Subscription {
		return []fism.Subscription{{Event: "test.runtime", MinVersion: 1, MaxVersion: 1, DependsOn: deps}}
	}
	runtime, err := fism.NewRuntime(s, s.EventBus(), []fism.EventDefinition{{ID: "test.runtime", Version: 1, Validate: fism.Object}}, []fism.Controller{
		{Definition: fism.ControllerDefinition{ID: "base", Subscriptions: subscription()}, PreExec: []fism.Step{record("pre")}, Exec: []fism.Step{record("exec"), func(ctx context.Context, _ fism.Event, _ *fism.StepContext) fism.Outcome {
			close(baseStarted)
			select {
			case <-gate:
			case <-ctx.Done():
			}
			return fism.Outcome{Kind: fism.Continue}
		}}, PostExec: []fism.Step{record("post")}},
		{Definition: fism.ControllerDefinition{ID: "other", Subscriptions: subscription()}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			close(otherStarted)
			return fism.Outcome{Kind: fism.Continue}
		}}},
		{Definition: fism.ControllerDefinition{ID: "dependent", Subscriptions: subscription("base")}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			close(dependentStarted)
			return record("dependent")(context.Background(), fism.Event{}, nil)
		}}},
	}, fism.RunConfig{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), fism.EmitRequest{ID: "test.runtime", Version: 1, Source: "test", Payload: []byte(`{}`)})
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
			runtime, err := fism.NewRuntime(s, s.EventBus(), []fism.EventDefinition{{ID: "test.retry", Version: 1, Validate: fism.Object}}, []fism.Controller{
				{Definition: fism.ControllerDefinition{ID: "retry", Subscriptions: []fism.Subscription{{Event: "test.retry", MinVersion: 1, MaxVersion: 1}}}, PreExec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
					preRuns.Add(1)
					return fism.Outcome{Kind: fism.Continue}
				}}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
					attempt := runs.Add(1)
					if attempt == 1 {
						if panicFirst {
							panic("offline")
						}
						return fism.Outcome{Kind: fism.Fail, Error: "offline"}
					}
					if attempt == 2 {
						close(second)
					}
					return fism.Outcome{Kind: fism.Continue}
				}}},
				{Definition: fism.ControllerDefinition{ID: "dependent", Subscriptions: []fism.Subscription{{Event: "test.retry", MinVersion: 1, MaxVersion: 1, DependsOn: []string{"retry"}}}}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
					dependentRuns.Add(1)
					return fism.Outcome{Kind: fism.Continue}
				}}},
			}, fism.RunConfig{Capacity: 1, Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
			if err != nil {
				t.Fatal(err)
			}
			event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), fism.EmitRequest{ID: "test.retry", Version: 1, Source: "test", Payload: []byte(`{}`)})
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
	fism.Journal
	failed chan struct{}
	once   sync.Once
}

func (j *flakyRouterFinish) FinishRouterDelivery(ctx context.Context, token string, r *fism.Registry, run fism.ClaimedRun, outcome fism.Outcome, now int64) error {
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
	runtime, err := fism.NewRuntime(journal, s.EventBus(), []fism.EventDefinition{{ID: "test.finish", Version: 1, Validate: fism.Object}}, []fism.Controller{{Definition: fism.ControllerDefinition{ID: "finish", Subscriptions: []fism.Subscription{{Event: "test.finish", MinVersion: 1, MaxVersion: 1}}}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
		executions.Add(1)
		return fism.Outcome{Kind: fism.Continue}
	}}}}, fism.RunConfig{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), fism.EmitRequest{ID: "test.finish", Version: 1, Source: "test", Payload: []byte(`{}`)})
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
	sub := func(deps ...string) []fism.Subscription {
		return []fism.Subscription{{Event: "test.stop", MinVersion: 1, MaxVersion: 1, DependsOn: deps}}
	}
	runtime, err := fism.NewRuntime(s, s.EventBus(), []fism.EventDefinition{{ID: "test.stop", Version: 1, Validate: fism.Object}}, []fism.Controller{
		{Definition: fism.ControllerDefinition{ID: "veto", Subscriptions: sub()}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			return fism.Outcome{Kind: fism.Stop}
		}}, PostExec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			postRuns.Add(1)
			return fism.Outcome{Kind: fism.Continue}
		}}},
		{Definition: fism.ControllerDefinition{ID: "dependent", Subscriptions: sub("veto")}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			dependentRuns.Add(1)
			return fism.Outcome{Kind: fism.Continue}
		}}},
		{Definition: fism.ControllerDefinition{ID: "independent", Subscriptions: sub()}, Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			return fism.Outcome{Kind: fism.Continue}
		}}},
	}, fism.RunConfig{Capacity: 3})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), fism.EmitRequest{ID: "test.stop", Version: 1, Source: "test", Payload: []byte(`{}`)})
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
	observed := make(chan fism.Event, 1)
	runtime, err := fism.NewRuntime(s, s.EventBus(), []fism.EventDefinition{{ID: "test.cause", Version: 1, Validate: fism.Object}, {ID: "test.effect", Version: 1, Validate: fism.Object}}, []fism.Controller{
		{Definition: fism.ControllerDefinition{ID: "producer", Subscriptions: []fism.Subscription{{Event: "test.cause", MinVersion: 1, MaxVersion: 1}}}, Exec: []fism.Step{func(ctx context.Context, _ fism.Event, step *fism.StepContext) fism.Outcome {
			if _, err := step.Emit(ctx, "test.effect", 1, []byte(`{"effect":true}`)); err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			return fism.Outcome{Kind: fism.Continue}
		}}},
		{Definition: fism.ControllerDefinition{ID: "observer", Subscriptions: []fism.Subscription{{Event: "test.effect", MinVersion: 1, MaxVersion: 1}}}, Exec: []fism.Step{func(ctx context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
			select {
			case observed <- event:
			case <-ctx.Done():
			}
			return fism.Outcome{Kind: fism.Continue}
		}}},
	}, fism.RunConfig{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	cause, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), fism.EmitRequest{ID: "test.cause", Version: 1, Source: "test", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	stop := runRouterFixture(t, runtime)
	var effect fism.Event
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
	defs := []fism.EventDefinition{{ID: "test.recover", Version: 1, Validate: fism.Object}}
	plugin := func(id string, limit uint32) fism.ControllerDefinition {
		return fism.ControllerDefinition{ID: id, MaxAttempts: limit, Subscriptions: []fism.Subscription{{Event: "test.recover", MinVersion: 1, MaxVersion: 1}}}
	}
	original, err := fism.NewRegistry(defs, []fism.ControllerDefinition{plugin("working", 0), plugin("removed", 0), plugin("exhausted", 1)})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(ctx, original, fism.EmitRequest{ID: "test.recover", Version: 1, Source: "test", Payload: []byte(`{}`)})
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
			if err := s.FinishRouterDelivery(ctx, token, original, run, fism.Outcome{Kind: fism.Fail, Error: "terminal"}, event.Event.EmittedAt); err != nil {
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
	current, err := fism.NewRegistry(defs, []fism.ControllerDefinition{plugin("working", 0), plugin("exhausted", 1)})
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
	states := map[string]fism.Delivery{}
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
	if err := reopened.FinishRouterDelivery(ctx, recoveredToken, current, batch.Runs[0], fism.Outcome{Kind: fism.Continue}, event.Event.EmittedAt+1); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ReleaseRouterRuntime(ctx, recoveredToken); err != nil {
		t.Fatal(err)
	}
}

func TestRouterRuntimeCancellationJoinsOwnedBlockedStep(t *testing.T) {
	s := testStore(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	runtime, err := fism.NewRuntime(s, s.EventBus(), []fism.EventDefinition{{ID: "test.shutdown", Version: 1, Validate: fism.Object}}, []fism.Controller{{Definition: fism.ControllerDefinition{ID: "blocked", Subscriptions: []fism.Subscription{{Event: "test.shutdown", MinVersion: 1, MaxVersion: 1}}}, Exec: []fism.Step{func(ctx context.Context, _ fism.Event, _ *fism.StepContext) fism.Outcome {
		close(started)
		<-ctx.Done()
		close(stopped)
		return fism.Outcome{Kind: fism.Continue}
	}}}}, fism.RunConfig{Capacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	event, err := s.EmitRouterEvent(context.Background(), runtime.Registry(), fism.EmitRequest{ID: "test.shutdown", Version: 1, Source: "test", Payload: []byte(`{}`)})
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
