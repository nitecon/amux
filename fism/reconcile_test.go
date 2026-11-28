package fism

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nitecon/amux/eventbus"
)

func TestReconcileEventWaitsForRequiredControllerOutcomes(t *testing.T) {
	const eventID = "question"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bus := eventbus.New()
	defer bus.Close()
	journal := &runtimeFakeJournal{}
	entered := make(chan string, 3)
	release := make(chan struct{})
	controller := func(id string, dependencies []string, step Step) Controller {
		return Controller{
			Definition: ControllerDefinition{ID: id, Subscriptions: []Subscription{{
				Event: eventID, MinVersion: 1, MaxVersion: 2, DependsOn: dependencies,
			}}},
			Exec: []Step{step},
		}
	}
	engine, err := NewRuntime(journal, bus, definitions(eventID, 1), []Controller{
		controller("fast", nil, func(context.Context, Event, *StepContext) Outcome {
			entered <- "fast"
			return Outcome{Kind: Continue}
		}),
		controller("slow", nil, func(ctx context.Context, _ Event, _ *StepContext) Outcome {
			entered <- "slow"
			select {
			case <-release:
				return Outcome{Kind: Continue}
			case <-ctx.Done():
				return Outcome{Kind: Fail}
			}
		}),
		controller("after", []string{"fast", "slow"}, func(context.Context, Event, *StepContext) Outcome {
			entered <- "after"
			return Outcome{Kind: Continue}
		}),
	}, RunConfig{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	if deliveries, err := engine.ReconcileEvent(ctx, Event{UID: "unknown-version", ID: eventID, Version: 2}); !errors.Is(err, ErrInvalidEvent) || len(deliveries) != 0 {
		t.Fatalf("undeclared event version returned deliveries=%+v error=%v", deliveries, err)
	}
	type result struct {
		deliveries []Delivery
		err        error
	}
	done := make(chan result, 1)
	go func() {
		deliveries, err := engine.ReconcileEvent(ctx, Event{UID: "fresh-event", ID: eventID, Version: 1})
		done <- result{deliveries, err}
	}()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-entered:
			if id == "after" {
				t.Fatal("dependent controller ran before its dependencies settled")
			}
			seen[id] = true
		case <-ctx.Done():
			t.Fatal("independent controllers did not run concurrently")
		}
	}
	select {
	case <-done:
		t.Fatal("pass returned before a required controller finished")
	default:
	}
	close(release)
	select {
	case result := <-done:
		if result.err != nil || len(result.deliveries) != 3 {
			t.Fatalf("reconcile returned %+v", result)
		}
		for _, delivery := range result.deliveries {
			if delivery.State != "completed" {
				t.Fatalf("delivery did not settle: %+v", delivery)
			}
		}
	case <-ctx.Done():
		t.Fatal("reconciliation did not settle")
	}
	if journal.claimCalls != 0 || journal.finishCalls != 0 {
		t.Fatal("in-memory progress was sent to the delivery journal")
	}
}

func TestReconcileEventRetriesOnlyUnfinishedControllers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bus := eventbus.New()
	defer bus.Close()
	journal := &runtimeFakeJournal{}
	var readyCalls, retryCalls int
	controller := func(id string, step Step) Controller {
		return Controller{
			Definition: ControllerDefinition{ID: id, MaxAttempts: 2, Subscriptions: []Subscription{{
				Event: "work", MinVersion: 1, MaxVersion: 1,
			}}},
			Exec: []Step{step},
		}
	}
	engine, err := NewRuntime(journal, bus, definitions("work", 1), []Controller{
		controller("ready", func(context.Context, Event, *StepContext) Outcome {
			readyCalls++
			return Outcome{Kind: Continue}
		}),
		controller("retry", func(context.Context, Event, *StepContext) Outcome {
			retryCalls++
			if retryCalls == 1 {
				return Outcome{Kind: Fail, Error: "transient"}
			}
			return Outcome{Kind: Continue}
		}),
	}, RunConfig{Capacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := engine.ReconcileEvent(ctx, Event{UID: "fresh-event", ID: "work", Version: 1})
	if err != nil || readyCalls != 1 || retryCalls != 2 || len(deliveries) != 2 {
		t.Fatalf("deliveries=%+v ready=%d retry=%d err=%v", deliveries, readyCalls, retryCalls, err)
	}
	if deliveries[1].State != "completed" || deliveries[1].Attempts != 1 || deliveries[1].NextAttemptAt != 0 {
		t.Fatalf("retry outcome was not settled: %+v", deliveries[1])
	}
	if journal.finishCalls != 0 {
		t.Fatal("retry progress was sent to the delivery journal")
	}
}
