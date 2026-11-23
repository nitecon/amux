package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const notifierPluginID = "notifier"

// NotifierStore provides settings and transactional notifier reconciliation.
type NotifierStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterNotifiers(context.Context, *router.Registry, router.Event) error
}

// NotifierPlugin reconciles notifications for task lifecycle events.
func NotifierPlugin(store NotifierStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: notifierPluginID,
			Subscriptions: []router.Subscription{{
				Event: "task.created", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "task.commented", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "task.user_review", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "task.completed", MinVersion: 1, MaxVersion: 1,
			}},
		},
		PreExec: []router.Step{enabledStep(store, notifierPluginID, "notifier plugin store is nil")},
		Exec: []router.Step{func(ctx context.Context, event router.Event, step *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "notifier plugin store is nil"}
			}
			if step == nil || step.Registry() == nil {
				return router.Outcome{Kind: router.Fail, Error: "notifier plugin registry is nil"}
			}
			if err := store.ReconcileRouterNotifiers(ctx, step.Registry(), event); err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
