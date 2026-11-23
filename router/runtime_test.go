package router

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
)

type runtimeFakeJournal struct {
	mu           sync.Mutex
	claimed      []ClaimedRun
	claimCalls   int
	finishCalls  int
	finishFn     func(ClaimedRun, Outcome, int) error
	emits        []EmitRequest
	acquired     chan struct{}
	released     chan struct{}
	acquireGate  <-chan struct{}
	claimStarted chan struct{}
	claimGate    <-chan struct{}
	claimOnce    sync.Once
	claimFailure error
	claimFailed  chan struct{}
}

func (j *runtimeFakeJournal) EmitRouterEvent(_ context.Context, _ *Registry, req EmitRequest) (EmitResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	req.Payload = append(json.RawMessage(nil), req.Payload...)
	if req.Correlation != nil {
		c := *req.Correlation
		req.Correlation = &c
	}
	j.emits = append(j.emits, req)
	return EmitResult{Stored: true}, nil
}

func (j *runtimeFakeJournal) AcquireRouterRuntime(ctx context.Context, _ *Registry, _ int64) (string, error) {
	if j.acquired != nil {
		close(j.acquired)
	}
	if j.acquireGate != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-j.acquireGate:
		}
	}
	return "token", nil
}

func (j *runtimeFakeJournal) ClaimRouterDeliveries(ctx context.Context, _ string, _ *Registry, _ int64, capacity int) (ClaimBatch, error) {
	if j.claimGate != nil {
		j.claimOnce.Do(func() {
			if j.claimStarted != nil {
				close(j.claimStarted)
			}
			select {
			case <-ctx.Done():
			case <-j.claimGate:
			}
		})
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.claimCalls++
	if j.claimFailure != nil {
		err := j.claimFailure
		j.claimFailure = nil
		if j.claimFailed != nil {
			close(j.claimFailed)
		}
		return ClaimBatch{}, err
	}
	if len(j.claimed) == 0 {
		return ClaimBatch{}, nil
	}
	count := min(capacity, len(j.claimed))
	runs := append([]ClaimedRun(nil), j.claimed[:count]...)
	j.claimed = j.claimed[count:]
	return ClaimBatch{Runs: runs}, nil
}

func (j *runtimeFakeJournal) FinishRouterDelivery(_ context.Context, _ string, _ *Registry, run ClaimedRun, outcome Outcome, _ int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.finishCalls++
	if j.finishFn != nil {
		return j.finishFn(run, outcome, j.finishCalls)
	}
	return nil
}

func (j *runtimeFakeJournal) ReleaseRouterRuntime(_ context.Context, _ string) error {
	if j.released != nil {
		close(j.released)
	}
	return nil
}

func testRuntime(t *testing.T, journal Journal, bus *eventbus.Bus, plugin Plugin, config RunConfig) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(journal, bus, definitions("work", 1), []Plugin{plugin}, config)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestRuntimeRunsPhasesInOrderAndCopiesStepEvents(t *testing.T) {
	event := Event{UID: "event-uid", ID: "work", Version: 1, Payload: json.RawMessage(`{"value":1}`), Correlation: ptrString("parent")}
	journal := &runtimeFakeJournal{claimed: []ClaimedRun{{Event: event, Plugin: "p"}}}
	bus := eventbus.New()
	defer bus.Close()
	var got []string
	step := func(name string) Step {
		return func(_ context.Context, event Event, _ *StepContext) Outcome {
			got = append(got, name)
			event.Payload[2] = 'X'
			if event.Correlation != nil {
				*event.Correlation = "mutated"
			}
			return Outcome{Kind: Continue}
		}
	}
	plugin := Plugin{Definition: PluginDefinition{ID: "p"}, PreExec: []Step{step("pre")}, Exec: []Step{step("exec")}, PostExec: []Step{step("post")}}
	runtime := testRuntime(t, journal, bus, plugin, RunConfig{Capacity: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if gotWant := []string{"pre", "exec", "post"}; !equalStrings(got, gotWant) {
		t.Fatalf("phase order = %v, want %v", got, gotWant)
	}
	if string(event.Payload) != `{"value":1}` || *event.Correlation != "parent" {
		t.Fatalf("step mutated original event: %#v", event)
	}
}

func TestRuntimeFinishRetryAcknowledgesWithoutReexecuting(t *testing.T) {
	var claimsBeforeAck int
	journal := &runtimeFakeJournal{}
	journal.claimed = []ClaimedRun{{Event: Event{UID: "uid", ID: "work", Version: 1}, Plugin: "p"}}
	journal.finishFn = func(_ ClaimedRun, _ Outcome, call int) error {
		if call == 1 {
			return errors.New("transient finish failure")
		}
		claimsBeforeAck = journal.claimCalls
		return nil
	}
	bus := eventbus.New()
	defer bus.Close()
	var steps int
	runtime := testRuntime(t, journal, bus, Plugin{Definition: PluginDefinition{ID: "p"}, Exec: []Step{func(context.Context, Event, *StepContext) Outcome {
		steps++
		return Outcome{Kind: Continue}
	}}}, RunConfig{Capacity: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if steps != 1 || journal.finishCalls != 2 {
		t.Fatalf("steps=%d finish calls=%d, want 1 and 2", steps, journal.finishCalls)
	}
	if claimsBeforeAck != 1 {
		t.Fatalf("claims=%d before finish acknowledgment, want 1", claimsBeforeAck)
	}
}

func TestRuntimeCapacityBoundsOwnedExecutions(t *testing.T) {
	journal := &runtimeFakeJournal{claimed: []ClaimedRun{
		{Event: Event{UID: "one", ID: "work", Version: 1}, Plugin: "p"},
		{Event: Event{UID: "two", ID: "work", Version: 1}, Plugin: "p"},
		{Event: Event{UID: "three", ID: "work", Version: 1}, Plugin: "p"},
	}}
	bus := eventbus.New()
	defer bus.Close()
	started, release := make(chan struct{}, 3), make(chan struct{})
	step := func(ctx context.Context, _ Event, _ *StepContext) Outcome {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Outcome{Kind: Continue}
	}
	runtime := testRuntime(t, journal, bus, Plugin{Definition: PluginDefinition{ID: "p"}, Exec: []Step{step}}, RunConfig{Capacity: 2})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("runtime did not start work up to its capacity")
		}
	}
	select {
	case <-started:
		t.Fatal("runtime exceeded configured capacity")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runtime did not admit queued work after a slot was freed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCapacityDefaultsAndCapsAt64(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()
	for _, test := range []struct {
		configured int
		want       int
	}{{configured: 0, want: 64}, {configured: 500, want: 64}} {
		runtime := testRuntime(t, &runtimeFakeJournal{}, bus, Plugin{Definition: PluginDefinition{ID: "p"}}, RunConfig{Capacity: test.configured})
		if runtime.capacity != test.want {
			t.Fatalf("capacity for config %d = %d, want %d", test.configured, runtime.capacity, test.want)
		}
	}
}

func TestRuntimeStepContextEmitsCorrelatedPluginEvent(t *testing.T) {
	journal := &runtimeFakeJournal{claimed: []ClaimedRun{{Event: Event{UID: "cause-uid", ID: "work", Version: 1}, Plugin: "p"}}}
	bus := eventbus.New()
	defer bus.Close()
	runtime := testRuntime(t, journal, bus, Plugin{Definition: PluginDefinition{ID: "p"}, Exec: []Step{func(ctx context.Context, _ Event, stepCtx *StepContext) Outcome {
		_, err := stepCtx.Emit(ctx, "work", 1, json.RawMessage(`{"emitted":true}`))
		if err != nil {
			return Outcome{Kind: Fail, Error: err.Error()}
		}
		return Outcome{Kind: Continue}
	}}}, RunConfig{Capacity: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := runtime.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(journal.emits) != 1 {
		t.Fatalf("emits = %d, want 1", len(journal.emits))
	}
	emit := journal.emits[0]
	if emit.Source != "plugin:p" || emit.Correlation == nil || *emit.Correlation != "cause-uid" || string(emit.Payload) != `{"emitted":true}` {
		t.Fatalf("unexpected emitted request: %#v", emit)
	}
}

func TestRuntimeTurnsPanicAndInvalidOutcomeIntoFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		step Step
	}{
		{name: "panic", step: func(context.Context, Event, *StepContext) Outcome { panic("broken") }},
		{name: "invalid outcome", step: func(context.Context, Event, *StepContext) Outcome { return Outcome{Kind: OutcomeKind(99)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outcome Outcome
			journal := &runtimeFakeJournal{claimed: []ClaimedRun{{Event: Event{UID: "uid", ID: "work", Version: 1}, Plugin: "p"}}}
			journal.finishFn = func(_ ClaimedRun, got Outcome, _ int) error { outcome = got; return nil }
			bus := eventbus.New()
			defer bus.Close()
			runtime := testRuntime(t, journal, bus, Plugin{Definition: PluginDefinition{ID: "p"}, Exec: []Step{tc.step}}, RunConfig{Capacity: 1})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := runtime.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if outcome.Kind != Fail {
				t.Fatalf("outcome = %#v, want Fail", outcome)
			}
		})
	}
}

func TestRuntimeRejectsConcurrentRunAndCancellationReleases(t *testing.T) {
	gate := make(chan struct{})
	journal := &runtimeFakeJournal{acquired: make(chan struct{}), released: make(chan struct{}), acquireGate: gate}
	bus := eventbus.New()
	defer bus.Close()
	runtime := testRuntime(t, journal, bus, Plugin{Definition: PluginDefinition{ID: "p"}}, RunConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	<-journal.acquired
	if err := runtime.Run(context.Background()); !errors.Is(err, ErrRuntimeRunning) {
		t.Fatalf("concurrent Run error = %v, want ErrRuntimeRunning", err)
	}
	close(gate)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("cancellation should be graceful, got %v", err)
	}
	select {
	case <-journal.released:
	case <-time.After(time.Second):
		t.Fatal("runtime token was not released")
	}
}

func TestRuntimeClaimErrorCanBeCanceledAndJoined(t *testing.T) {
	journal := &runtimeFakeJournal{
		claimFailure: errors.New("temporary claim failure"),
		claimFailed:  make(chan struct{}),
		released:     make(chan struct{}),
	}
	bus := eventbus.New()
	defer bus.Close()
	runtime := testRuntime(t, journal, bus, Plugin{Definition: PluginDefinition{ID: "p"}}, RunConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	<-journal.claimFailed
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation should be graceful: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not join after transient claim error")
	}
	select {
	case <-journal.released:
	case <-time.After(time.Second):
		t.Fatal("runtime did not release its token")
	}
}

func TestRuntimeResubscribesAfterChannelOverflow(t *testing.T) {
	entered, unblock := make(chan struct{}), make(chan struct{})
	journal := &runtimeFakeJournal{claimStarted: entered, claimGate: unblock}
	// Hold the first claim so the runtime cannot drain its bounded subscription.
	bus := eventbus.New()
	defer bus.Close()
	runtime := testRuntime(t, journal, bus, Plugin{Definition: PluginDefinition{ID: "p"}}, RunConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	<-entered
	for i := 0; i < runtimeBusBuffer+1; i++ {
		if err := bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged}); err != nil {
			t.Fatal(err)
		}
	}
	close(unblock)
	deadline := time.Now().Add(time.Second)
	for {
		journal.mu.Lock()
		calls := journal.claimCalls
		journal.mu.Unlock()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime did not reconcile after resubscribing")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func ptrString(value string) *string { return &value }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
