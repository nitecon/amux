package router

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
)

func TestRuntimeRoutesClaimedWorkToPluginChannels(t *testing.T) {
	journal := &runtimeFakeJournal{claimed: []ClaimedRun{
		{Event: Event{UID: "for-a", ID: "work", Version: 1}, Plugin: "a"},
		{Event: Event{UID: "for-b", ID: "work", Version: 1}, Plugin: "b"},
	}}
	bus := eventbus.New()
	defer bus.Close()
	gotA, gotB := make(chan string, 1), make(chan string, 1)
	plugin := func(id string, got chan<- string) Plugin {
		return Plugin{Definition: PluginDefinition{ID: id, Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 1}}}, Exec: []Step{
			func(_ context.Context, event Event, _ *StepContext) Outcome {
				got <- event.UID
				return Outcome{Kind: Continue}
			},
		}}
	}
	runtime, err := NewRuntime(journal, bus, definitions("work", 1), []Plugin{plugin("a", gotA), plugin("b", gotB)}, RunConfig{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	for name, ch := range map[string]<-chan string{"a": gotA, "b": gotB} {
		select {
		case uid := <-ch:
			if want := "for-" + name; uid != want {
				t.Errorf("plugin %s received %q, want %q", name, uid, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("plugin %s did not receive its claimed work", name)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDrainsQueuedPluginChannelOnShutdown(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()
	var callbackCalls atomic.Int32
	runtime := testRuntime(t, &runtimeFakeJournal{}, bus, Plugin{Definition: PluginDefinition{ID: "p"}, Exec: []Step{
		func(context.Context, Event, *StepContext) Outcome {
			callbackCalls.Add(1)
			return Outcome{Kind: Continue}
		},
	}}, RunConfig{Capacity: 2})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	completed := make(chan *activeRun, 2)
	var admissionMu sync.Mutex
	pendingWrites := 0
	inbox := make(chan *activeRun, 2)
	runs := []*activeRun{
		{claimed: ClaimedRun{Event: Event{UID: "queued-1", ID: "work", Version: 1}, Plugin: "p"}, finished: make(chan struct{})},
		{claimed: ClaimedRun{Event: Event{UID: "queued-2", ID: "work", Version: 1}, Plugin: "p"}, finished: make(chan struct{})},
	}
	for _, run := range runs {
		inbox <- run
	}
	close(inbox)
	var dispatchers sync.WaitGroup
	dispatchers.Add(1)
	runtime.dispatchPlugin(ctx, inbox, completed, &admissionMu, &pendingWrites, &dispatchers)
	dispatchers.Wait()
	for _, run := range runs {
		select {
		case <-run.finished:
		case <-time.After(time.Second):
			t.Fatalf("queued run %s did not finish during shutdown drain", run.claimed.Event.UID)
		}
	}
	completedRuns := make(map[*activeRun]bool, len(runs))
	for range runs {
		completedRuns[<-completed] = true
	}
	for _, run := range runs {
		if !completedRuns[run] {
			t.Errorf("queued run %s was not completed", run.claimed.Event.UID)
		}
	}
	if calls := callbackCalls.Load(); calls != 0 {
		t.Fatalf("plugin callbacks after canceled queued drain = %d, want 0", calls)
	}
}
