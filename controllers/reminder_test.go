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

type reminderStoreFunc struct {
	enabled   func(context.Context, string) (bool, error)
	reconcile func(context.Context, *fism.Registry, fism.Event) error
}

func (s reminderStoreFunc) PluginEnabled(ctx context.Context, id string) (bool, error) {
	return s.enabled(ctx, id)
}

func (s reminderStoreFunc) ReconcileRouterReminders(ctx context.Context, registry *fism.Registry, event fism.Event) error {
	return s.reconcile(ctx, registry, event)
}

func TestReminderHeartbeatIgnoresPeriodicSettingAndForwardsEvent(t *testing.T) {
	cause := fism.Event{Seq: 7, UID: "heartbeat", ID: ClientHeartbeatID, Version: 1, Payload: []byte(`{"agents":[],"client_run":"123e4567-e89b-12d3-a456-426614174000"}`), Source: "client:one", EmittedAt: 1234}
	enabledCalls := 0
	var gotEvent fism.Event
	var gotRegistry *fism.Registry
	store := reminderStoreFunc{
		enabled: func(context.Context, string) (bool, error) {
			enabledCalls++
			return false, nil
		},
		reconcile: func(_ context.Context, registry *fism.Registry, event fism.Event) error {
			gotRegistry, gotEvent = registry, event
			return nil
		},
	}
	journal := &assignmentRuntimeJournal{event: cause, plugins: []string{reminderPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime, err := fism.NewRuntime(journal, eventbus.New(), presenceTestDefinitions(), []fism.Controller{
		PresenceController(nil), ReminderController(store),
	}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case got := <-journal.finished:
		if got.outcome.Kind != fism.Continue {
			t.Fatalf("heartbeat outcome=%+v, want Continue", got.outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("reminder did not complete heartbeat event")
	}
	cancelAssignmentRuntime(t, cancel, done)
	if enabledCalls != 0 {
		t.Fatalf("PluginEnabled called %d times for heartbeat", enabledCalls)
	}
	if gotRegistry != runtime.Registry() || !reflect.DeepEqual(gotEvent, cause) {
		t.Fatalf("reminder got registry=%p event=%+v, want registry=%p event=%+v", gotRegistry, gotEvent, runtime.Registry(), cause)
	}
}

func TestReminderClockUsesEnabledStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    fism.OutcomeKind
	}{
		{name: "disabled", enabled: false, want: fism.Stop},
		{name: "enabled", enabled: true, want: fism.Continue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			store := reminderStoreFunc{
				enabled: func(_ context.Context, id string) (bool, error) {
					if id != reminderPluginID {
						t.Errorf("enabled plugin=%q, want %q", id, reminderPluginID)
					}
					return tc.enabled, nil
				},
				reconcile: func(context.Context, *fism.Registry, fism.Event) error { calls++; return nil },
			}
			journal := &assignmentRuntimeJournal{event: fism.Event{ID: "clock.tick", Version: 1}, plugins: []string{reminderPluginID}, finished: make(chan assignmentFinish, 1)}
			runtime, err := fism.NewRuntime(journal, eventbus.New(), presenceTestDefinitions(), []fism.Controller{
				PresenceController(nil), ReminderController(store),
			}, fism.RunConfig{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runtime.Run(ctx) }()
			select {
			case got := <-journal.finished:
				if got.outcome.Kind != tc.want {
					t.Fatalf("clock outcome=%+v, want kind %v", got.outcome, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("reminder did not complete clock event")
			}
			cancelAssignmentRuntime(t, cancel, done)
			wantCalls := 0
			if tc.enabled {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("reconcile calls=%d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestReminderNilRegistryAndStoreErrorsFail(t *testing.T) {
	step := ReminderController(reminderStoreFunc{
		enabled: func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(context.Context, *fism.Registry, fism.Event) error {
			return errors.New("reminder reconcile failed")
		},
	}).Exec[0]
	if got := step(context.Background(), fism.Event{}, nil); got.Kind != fism.Fail || got.Error != "reminder plugin registry is nil" {
		t.Fatalf("nil step context outcome=%+v", got)
	}
	if got := ReminderController(nil).Exec[0](context.Background(), fism.Event{}, nil); got.Kind != fism.Fail || got.Error != "reminder plugin store is nil" {
		t.Fatalf("nil store outcome=%+v", got)
	}
	cause := fism.Event{ID: ClientHeartbeatID, Version: 1, Payload: []byte(`{"agents":[]}`)}
	journal := &assignmentRuntimeJournal{event: cause, plugins: []string{reminderPluginID}, finished: make(chan assignmentFinish, 1)}
	runtime, err := fism.NewRuntime(journal, eventbus.New(), presenceTestDefinitions(), []fism.Controller{
		PresenceController(nil), ReminderController(reminderStoreFunc{
			enabled: func(context.Context, string) (bool, error) { return false, nil },
			reconcile: func(context.Context, *fism.Registry, fism.Event) error {
				return errors.New("reminder reconcile failed")
			},
		}),
	}, fism.RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	select {
	case got := <-journal.finished:
		if got.outcome.Kind != fism.Fail || got.outcome.Error != "reminder reconcile failed" {
			t.Fatalf("store error outcome=%+v", got.outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("reminder did not report store error")
	}
	cancelAssignmentRuntime(t, cancel, done)
}
