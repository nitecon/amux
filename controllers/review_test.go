package controllers

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/nitecon/amux/fism"
)

type reviewStoreFunc struct {
	enabled   func(context.Context, string) (bool, error)
	reconcile func(context.Context, fism.Event) (bool, error)
}

func (s reviewStoreFunc) PluginEnabled(ctx context.Context, plugin string) (bool, error) {
	return s.enabled(ctx, plugin)
}

func (s reviewStoreFunc) ReconcileRouterReviews(ctx context.Context, event fism.Event) (bool, error) {
	return s.reconcile(ctx, event)
}

func TestReviewPluginMetadata(t *testing.T) {
	plugin := ReviewController(reviewStoreFunc{})
	if plugin.Definition.ID != reviewPluginID || plugin.Definition.FIFO || plugin.Definition.MaxAttempts != 0 {
		t.Fatalf("unexpected plugin definition: %+v", plugin.Definition)
	}
	if len(plugin.Definition.Subscriptions) != 1 {
		t.Fatalf("subscriptions=%+v, want one", plugin.Definition.Subscriptions)
	}
	sub := plugin.Definition.Subscriptions[0]
	if sub.Event != "task.completed" || sub.MinVersion != 1 || sub.MaxVersion != 1 || len(sub.DependsOn) != 0 {
		t.Fatalf("unexpected subscription: %+v", sub)
	}
	if len(plugin.PreExec) != 1 || len(plugin.Exec) != 1 || len(plugin.PostExec) != 0 {
		t.Fatalf("unexpected plugin phases: pre=%d exec=%d post=%d", len(plugin.PreExec), len(plugin.Exec), len(plugin.PostExec))
	}
}

func TestReviewPluginPreExecChecksSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store ReviewStore
		want  fism.Outcome
	}{
		{name: "enabled", store: reviewStoreFunc{enabled: func(_ context.Context, plugin string) (bool, error) { return plugin == reviewPluginID, nil }}, want: fism.Outcome{Kind: fism.Continue}},
		{name: "disabled", store: reviewStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, nil }}, want: fism.Outcome{Kind: fism.Stop}},
		{name: "settings error", store: reviewStoreFunc{enabled: func(context.Context, string) (bool, error) { return false, errors.New("settings unavailable") }}, want: fism.Outcome{Kind: fism.Fail, Error: "settings unavailable"}},
		{name: "nil store", want: fism.Outcome{Kind: fism.Fail, Error: "review plugin store is nil"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := ReviewController(tc.store)
			outcome := plugin.PreExec[0](context.Background(), fism.Event{}, nil)
			if outcome != tc.want {
				t.Fatalf("outcome=%+v, want %+v", outcome, tc.want)
			}
		})
	}
}

func TestReviewPluginExecForwardsEventAndMapsReconcileResult(t *testing.T) {
	event := fism.Event{Seq: 73, UID: "completed-uid", ID: "task.completed", Version: 1, Payload: []byte(`{"task_id":"task-9"}`), Source: "tasks", EmittedAt: 1723456789012}
	for _, tc := range []struct {
		name     string
		reviewed bool
		err      error
		want     fism.Outcome
	}{
		{name: "reviewed", reviewed: true, want: fism.Outcome{Kind: fism.Continue}},
		{name: "no review", reviewed: false, want: fism.Outcome{Kind: fism.Stop}},
		{name: "store error", err: errors.New("review reconciliation failed"), want: fism.Outcome{Kind: fism.Fail, Error: "review reconciliation failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotEvent fism.Event
			store := reviewStoreFunc{
				enabled: func(context.Context, string) (bool, error) { return true, nil },
				reconcile: func(_ context.Context, got fism.Event) (bool, error) {
					gotEvent = got
					return tc.reviewed, tc.err
				},
			}
			outcome := ReviewController(store).Exec[0](context.Background(), event, nil)
			if outcome != tc.want {
				t.Fatalf("outcome=%+v, want %+v", outcome, tc.want)
			}
			if !reflect.DeepEqual(gotEvent, event) {
				t.Fatalf("reconcile event=%+v, want unchanged event %+v", gotEvent, event)
			}
		})
	}
}

func TestReviewPluginPreExecPassesCanceledContextToSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plugin := ReviewController(reviewStoreFunc{
		enabled: func(ctx context.Context, _ string) (bool, error) {
			return false, ctx.Err()
		},
	})
	outcome := plugin.PreExec[0](ctx, fism.Event{}, nil)
	if outcome != (fism.Outcome{Kind: fism.Fail, Error: context.Canceled.Error()}) {
		t.Fatalf("outcome=%+v, want cancellation failure", outcome)
	}
}
