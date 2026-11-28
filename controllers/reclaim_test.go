package controllers

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nitecon/amux/fism"
)

type reclaimStoreFunc struct {
	enabled   func(context.Context, string) (bool, error)
	reconcile func(context.Context, fism.Event) (int, error)
}

func (s reclaimStoreFunc) ReconcileRouterProgress(ctx context.Context, _ *fism.Registry, event fism.Event) error {
	_, err := s.reconcile(ctx, event)
	return err
}

func (s reclaimStoreFunc) PluginEnabled(ctx context.Context, id string) (bool, error) {
	return s.enabled(ctx, id)
}

func TestReclaimPluginMetadataAndPreExec(t *testing.T) {
	plugin := ReclaimController(reclaimStoreFunc{})
	if plugin.Definition.ID != reclaimPluginID || len(plugin.Definition.Subscriptions) != 1 {
		t.Fatalf("unexpected plugin definition: %+v", plugin.Definition)
	}
	sub := plugin.Definition.Subscriptions[0]
	if sub.Event != "clock.tick" || sub.MinVersion != 1 || sub.MaxVersion != 1 || !reflect.DeepEqual(sub.DependsOn, []string{"presence"}) {
		t.Fatalf("unexpected subscription: %+v", sub)
	}
	if len(plugin.PreExec) != 1 || len(plugin.Exec) != 1 || len(plugin.PostExec) != 0 {
		t.Fatalf("unexpected phases: pre=%d exec=%d post=%d", len(plugin.PreExec), len(plugin.Exec), len(plugin.PostExec))
	}
	for _, tc := range []struct {
		name  string
		store ReclaimStore
		want  fism.Outcome
	}{
		{name: "enabled", store: reclaimStoreFunc{enabled: func(_ context.Context, id string) (bool, error) { return id == reclaimPluginID, nil }}, want: fism.Outcome{Kind: fism.Continue}},
		{name: "disabled", store: reclaimStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, nil }}, want: fism.Outcome{Kind: fism.Stop}},
		{name: "settings error", store: reclaimStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, errors.New("settings unavailable") }}, want: fism.Outcome{Kind: fism.Fail, Error: "settings unavailable"}},
		{name: "nil store", want: fism.Outcome{Kind: fism.Fail, Error: "reclaim plugin store is nil"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ReclaimController(tc.store).PreExec[0](context.Background(), fism.Event{}, nil)
			if got != tc.want {
				t.Fatalf("outcome=%+v want %+v", got, tc.want)
			}
		})
	}
}

func TestReclaimPluginExecForwardsAndContinuesOnEmpty(t *testing.T) {
	event := fism.Event{ID: "clock.tick", Version: 1}
	called := false
	plugin := ReclaimController(reclaimStoreFunc{
		enabled: func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(_ context.Context, got fism.Event) (int, error) {
			called = true
			if got.ID != event.ID || got.Version != event.Version {
				t.Fatalf("event=%+v", got)
			}
			return 0, nil
		},
	})
	if got := plugin.Exec[0](context.Background(), event, nil); got.Kind != fism.Continue || !called {
		t.Fatalf("outcome=%+v called=%v", got, called)
	}
	failed := ReclaimController(reclaimStoreFunc{
		enabled:   func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(context.Context, fism.Event) (int, error) { return 0, errors.New("reconcile failed") },
	})
	if got := failed.Exec[0](context.Background(), event, nil); got != (fism.Outcome{Kind: fism.Fail, Error: "reconcile failed"}) {
		t.Fatalf("failure outcome=%+v", got)
	}
}
