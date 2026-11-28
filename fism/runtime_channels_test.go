package fism

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
)

func TestRuntimeRoutesClaimedWorkToPluginTopics(t *testing.T) {
	journal := &runtimeFakeJournal{claimed: []ClaimedRun{
		{Event: Event{UID: "for-a", ID: "work", Version: 1}, Plugin: "a"},
		{Event: Event{UID: "for-b", ID: "work", Version: 1}, Plugin: "b"},
	}}
	bus := eventbus.New()
	defer bus.Close()
	gotA, gotB := make(chan string, 1), make(chan string, 1)
	plugin := func(id string, got chan<- string) Controller {
		return Controller{Definition: ControllerDefinition{ID: id, Subscriptions: []Subscription{{Event: "work", MinVersion: 1, MaxVersion: 1}}}, Exec: []Step{
			func(_ context.Context, event Event, _ *StepContext) Outcome {
				got <- event.UID
				return Outcome{Kind: Continue}
			},
		}}
	}
	runtime, err := NewRuntime(journal, bus, definitions("work", 1), []Controller{plugin("a", gotA), plugin("b", gotB)}, RunConfig{Capacity: 2})
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

func TestRuntimeReleasesClaimsReturnedDuringShutdown(t *testing.T) {
	journal := &runtimeFakeJournal{
		claimed: []ClaimedRun{
			{Event: Event{UID: "queued", ID: "work", Version: 1}, Plugin: "p"},
		},
		claimStarted: make(chan struct{}),
		claimGate:    make(chan struct{}),
		released:     make(chan struct{}),
	}
	bus := eventbus.New()
	defer bus.Close()
	var callbackCalls atomic.Int32
	runtime := testRuntime(t, journal, bus, Controller{Definition: ControllerDefinition{ID: "p"}, Exec: []Step{
		func(context.Context, Event, *StepContext) Outcome {
			callbackCalls.Add(1)
			return Outcome{Kind: Continue}
		},
	}}, RunConfig{Capacity: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case <-journal.claimStarted:
	case <-ctx.Done():
		t.Fatal("runtime did not start claiming")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime did not finish shutdown")
	}
	select {
	case <-journal.released:
	default:
		t.Fatal("runtime did not release its journal claims")
	}
	if calls := callbackCalls.Load(); calls != 0 {
		t.Fatalf("plugin callbacks during canceled admission = %d, want 0", calls)
	}
}
