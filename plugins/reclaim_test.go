package plugins

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nitecon/amux/router"
)

type reclaimStoreFunc struct {
	enabled   func(context.Context, string) (bool, error)
	reconcile func(context.Context, router.Event) (int, error)
}

func (s reclaimStoreFunc) PluginEnabled(ctx context.Context, id string) (bool, error) {
	return s.enabled(ctx, id)
}

func (s reclaimStoreFunc) ReconcileRouterReclaim(ctx context.Context, event router.Event) (int, error) {
	return s.reconcile(ctx, event)
}

func TestReclaimPluginMetadataAndPreExec(t *testing.T) {
	plugin := ReclaimPlugin(reclaimStoreFunc{})
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
		want  router.Outcome
	}{
		{name: "enabled", store: reclaimStoreFunc{enabled: func(_ context.Context, id string) (bool, error) { return id == reclaimPluginID, nil }}, want: router.Outcome{Kind: router.Continue}},
		{name: "disabled", store: reclaimStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, nil }}, want: router.Outcome{Kind: router.Stop}},
		{name: "settings error", store: reclaimStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, errors.New("settings unavailable") }}, want: router.Outcome{Kind: router.Fail, Error: "settings unavailable"}},
		{name: "nil store", want: router.Outcome{Kind: router.Fail, Error: "reclaim plugin store is nil"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ReclaimPlugin(tc.store).PreExec[0](context.Background(), router.Event{}, nil)
			if got != tc.want {
				t.Fatalf("outcome=%+v want %+v", got, tc.want)
			}
		})
	}
}

func TestReclaimPluginExecForwardsAndContinuesOnEmpty(t *testing.T) {
	event := router.Event{ID: "clock.tick", Version: 1}
	called := false
	plugin := ReclaimPlugin(reclaimStoreFunc{
		enabled: func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(_ context.Context, got router.Event) (int, error) {
			called = true
			if got.ID != event.ID || got.Version != event.Version {
				t.Fatalf("event=%+v", got)
			}
			return 0, nil
		},
	})
	if got := plugin.Exec[0](context.Background(), event, nil); got.Kind != router.Continue || !called {
		t.Fatalf("outcome=%+v called=%v", got, called)
	}
	failed := ReclaimPlugin(reclaimStoreFunc{
		enabled:   func(context.Context, string) (bool, error) { return true, nil },
		reconcile: func(context.Context, router.Event) (int, error) { return 0, errors.New("reconcile failed") },
	})
	if got := failed.Exec[0](context.Background(), event, nil); got != (router.Outcome{Kind: router.Fail, Error: "reconcile failed"}) {
		t.Fatalf("failure outcome=%+v", got)
	}
}
