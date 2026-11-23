package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

type taskDrainerFunc func(context.Context, *router.Registry, int) (int, error)

func (f taskDrainerFunc) DrainRouterTaskOutbox(ctx context.Context, r *router.Registry, limit int) (int, error) {
	return f(ctx, r, limit)
}

type eventEmitterFunc func(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error)

func (f eventEmitterFunc) EmitRouterEvent(ctx context.Context, r *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
	return f(ctx, r, request)
}

func sourceRegistry(t *testing.T) *router.Registry {
	t.Helper()
	r, err := router.NewRegistry([]router.EventDefinition{ClockDefinition()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func startTaskSource(ctx context.Context, drainer TaskDrainer, r *router.Registry, bus *eventbus.Bus) <-chan error {
	done := make(chan error, 1)
	go func() { done <- RunTaskSource(ctx, drainer, r, bus) }()
	return done
}

func startClockSource(ctx context.Context, emitter EventEmitter, r *router.Registry, bus *eventbus.Bus, config ClockConfig) <-chan error {
	sub := bus.Subscribe(sourceSubscriptionBuffer)
	now := config.Now
	if now == nil {
		now = time.Now
	}
	done := make(chan error, 1)
	go func() { done <- runClockSource(ctx, emitter, r, bus, sub, now) }()
	return done
}

func stopSource(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("source returned on cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source did not stop after cancellation")
	}
}

func TestTaskSourceSubscribesBeforeStartupDrain(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	done := startTaskSource(ctx, taskDrainerFunc(func(context.Context, *router.Registry, int) (int, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			close(started)
			<-release
		}
		return 0, nil
	}), sourceRegistry(t), bus)
	<-started
	if err := bus.Publish(eventbus.Event{Topic: eventbus.TaskChanged}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 2 })
	stopSource(t, cancel, done)
}

func TestTaskSourceDrainsFullBatchesImmediately(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var limits, results []int
	done := startTaskSource(ctx, taskDrainerFunc(func(_ context.Context, _ *router.Registry, limit int) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		limits = append(limits, limit)
		results = append(results, len(results))
		if len(results) < 3 {
			return limit, nil
		}
		return 0, nil
	}), sourceRegistry(t), bus)
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(results) == 3 })
	mu.Lock()
	for _, limit := range limits {
		if limit != sourceDrainBatchSize {
			t.Fatalf("drain limit=%d, want %d", limit, sourceDrainBatchSize)
		}
	}
	mu.Unlock()
	stopSource(t, cancel, done)
}

func TestTaskSourceRetryCanBeWokenAndIgnoresRouterChanged(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 4)
	drainCalls := 0
	done := startTaskSource(ctx, taskDrainerFunc(func(context.Context, *router.Registry, int) (int, error) {
		drainCalls++
		calls <- struct{}{}
		if drainCalls == 1 {
			return 0, errors.New("temporary storage failure")
		}
		return 0, nil
	}), sourceRegistry(t), bus)
	<-calls
	if err := bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
		t.Fatal("RouterChanged caused a task outbox retry")
	case <-time.After(30 * time.Millisecond):
	}
	if err := bus.Publish(eventbus.Event{Topic: eventbus.TaskChanged}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("TaskChanged did not wake a failed drain")
	}
	stopSource(t, cancel, done)
}

func TestTaskSourceIrrelevantTrafficDoesNotPostponeRetry(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := make(chan struct{}, 3)
	drainCalls := 0
	done := startTaskSource(ctx, taskDrainerFunc(func(context.Context, *router.Registry, int) (int, error) {
		drainCalls++
		calls <- struct{}{}
		if drainCalls == 1 {
			return 0, errors.New("temporary storage failure")
		}
		return 0, nil
	}), sourceRegistry(t), bus)
	<-calls

	stopNoise := make(chan struct{})
	noiseDone := make(chan struct{})
	go func() {
		defer close(noiseDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopNoise:
				return
			case <-ticker.C:
				_ = bus.Publish(eventbus.Event{Topic: eventbus.ConversationChanged})
				_ = bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
			}
		}
	}()
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		close(stopNoise)
		<-noiseDone
		t.Fatal("irrelevant bus traffic postponed the scheduled retry")
	}
	close(stopNoise)
	<-noiseDone
	stopSource(t, cancel, done)
}

func TestTaskSourceOverflowResubscribesAndObservesClosedBus(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	done := startTaskSource(ctx, taskDrainerFunc(func(callCtx context.Context, _ *router.Registry, _ int) (int, error) {
		calls++
		if calls == 1 {
			close(entered)
			select {
			case <-release:
			case <-callCtx.Done():
			}
		}
		return 0, nil
	}), sourceRegistry(t), bus)
	<-entered
	for i := 0; i < sourceSubscriptionBuffer+1; i++ {
		if err := bus.Publish(eventbus.Event{Topic: eventbus.TaskChanged}); err != nil {
			t.Fatal(err)
		}
	}
	// The overflowed subscription keeps ErrSlowConsumer even after Bus.Close.
	// Only resubscribing can observe ErrBusClosed, so this is a deterministic
	// readiness assertion rather than a delay or a count of buffered hints.
	bus.Close()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, eventbus.ErrBusClosed) {
			t.Fatalf("resubscription error %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source did not resubscribe after overflow")
	}
	if calls < 2 {
		t.Fatal("source did not reconcile retained work after losing hints")
	}
}

func TestClockDefinition(t *testing.T) {
	definition := ClockDefinition()
	if definition.ID != "clock.tick" || definition.Version != 1 || definition.RemoteEmit {
		t.Fatalf("unexpected clock definition: %+v", definition)
	}
	if err := definition.Validate(json.RawMessage(`{"at":1}`)); err != nil {
		t.Fatalf("clock validator rejected object: %v", err)
	}
	if err := definition.Validate(json.RawMessage(`[]`)); err == nil {
		t.Fatal("clock validator accepted non-object payload")
	}
}

func TestClockSourceHasNoStartupEmitAndUsesTickProvenance(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emitted := make(chan router.EmitRequest, 2)
	now := time.UnixMilli(1_723_456_789_012)
	done := startClockSource(ctx, eventEmitterFunc(func(_ context.Context, _ *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
		emitted <- request
		return router.EmitResult{Stored: false}, nil // Storage skips when no plugin subscribes.
	}), sourceRegistry(t), bus, ClockConfig{Now: func() time.Time { return now }})
	select {
	case <-emitted:
		t.Fatal("clock source emitted during startup")
	case <-time.After(30 * time.Millisecond):
	}
	for _, topic := range []eventbus.Topic{eventbus.TaskChanged, eventbus.RouterChanged} {
		if err := bus.Publish(eventbus.Event{Topic: topic}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-emitted:
		t.Fatal("non-Tick notification caused a clock event")
	case <-time.After(30 * time.Millisecond):
	}
	if err := bus.Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-emitted:
		if request.ID != "clock.tick" || request.Version != 1 || request.Source != "clock" || request.UID != "" || request.Correlation != nil {
			t.Fatalf("unexpected clock request: %+v", request)
		}
		var payload struct {
			At int64 `json:"at"`
		}
		if err := json.Unmarshal(request.Payload, &payload); err != nil || payload.At != now.UnixMilli() {
			t.Fatalf("payload=%s at=%d err=%v", request.Payload, payload.At, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Tick did not emit clock event")
	}
	stopSource(t, cancel, done)
}

func TestClockSourceCoalescesQueuedTicksAndDropsFailedTick(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	requests := make(chan router.EmitRequest, 5)
	calls := 0
	done := startClockSource(ctx, eventEmitterFunc(func(_ context.Context, _ *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
		calls++
		requests <- request
		if calls == 1 {
			close(entered)
			<-release
			return router.EmitResult{}, errors.New("temporary emit failure")
		}
		return router.EmitResult{}, nil
	}), sourceRegistry(t), bus, ClockConfig{Now: func() time.Time { return time.UnixMilli(9) }})
	if err := bus.Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
		t.Fatal(err)
	}
	<-entered
	for i := 0; i < 5; i++ {
		if err := bus.Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	<-requests
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("queued ticks were not coalesced into one subsequent emit")
	}
	select {
	case <-requests:
		t.Fatal("queued ticks caused a catch-up burst")
	case <-time.After(30 * time.Millisecond):
	}
	if err := bus.Publish(eventbus.Event{Topic: eventbus.Tick}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("source did not emit on the next Tick after failure")
	}
	stopSource(t, cancel, done)
}

func TestClockSourceOverflowWaitsForSubsequentTick(t *testing.T) {
	bus := eventbus.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	requests := make(chan struct{}, 3)
	done := startClockSource(ctx, eventEmitterFunc(func(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error) {
		requests <- struct{}{}
		if len(requests) == 1 {
			close(entered)
			<-release
		}
		return router.EmitResult{}, nil
	}), sourceRegistry(t), bus, ClockConfig{})
	_ = bus.Publish(eventbus.Event{Topic: eventbus.Tick})
	<-entered
	for i := 0; i < sourceSubscriptionBuffer+1; i++ {
		_ = bus.Publish(eventbus.Event{Topic: eventbus.TaskChanged})
	}
	close(release)
	<-requests
	select {
	case <-requests:
		t.Fatal("overflow recovery fabricated a clock event")
	case <-time.After(30 * time.Millisecond):
	}
	if err := bus.Publish(eventbus.Event{Topic: eventbus.TaskChanged}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
		t.Fatal("non-Tick notification after overflow emitted a clock event")
	case <-time.After(30 * time.Millisecond):
	}
	_ = bus.Publish(eventbus.Event{Topic: eventbus.Tick})
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("source did not resume after subsequent Tick")
	}
	stopSource(t, cancel, done)
}

func TestSourcesReturnBusClosedUnlessCanceled(t *testing.T) {
	t.Run("task", func(t *testing.T) {
		bus := eventbus.New()
		done := startTaskSource(context.Background(), taskDrainerFunc(func(context.Context, *router.Registry, int) (int, error) { return 0, nil }), sourceRegistry(t), bus)
		bus.Close()
		select {
		case err := <-done:
			if !errors.Is(err, eventbus.ErrBusClosed) {
				t.Fatalf("error=%v, want ErrBusClosed", err)
			}
		case <-time.After(time.Second):
			t.Fatal("task source did not return after bus close")
		}
	})
	t.Run("clock", func(t *testing.T) {
		bus := eventbus.New()
		done := startClockSource(context.Background(), eventEmitterFunc(func(context.Context, *router.Registry, router.EmitRequest) (router.EmitResult, error) {
			return router.EmitResult{}, nil
		}), sourceRegistry(t), bus, ClockConfig{})
		bus.Close()
		select {
		case err := <-done:
			if !errors.Is(err, eventbus.ErrBusClosed) {
				t.Fatalf("error=%v, want ErrBusClosed", err)
			}
		case <-time.After(time.Second):
			t.Fatal("clock source did not return after bus close")
		}
	})
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition was not met")
		case <-tick.C:
		}
	}
}
