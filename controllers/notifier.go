package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
)

const notifierPluginID = "notifier"

// NotifierStore provides settings and transactional notifier reconciliation.
type NotifierStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterNotifiers(context.Context, *fism.Registry, fism.Event) error
}

// NotifierController reconciles notifications for task lifecycle events.
func NotifierController(store NotifierStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: notifierPluginID,
			Subscriptions: []fism.Subscription{{
				Event: "task.created", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "task.commented", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "task.user_review", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "task.completed", MinVersion: 1, MaxVersion: 1,
			}},
		},
		PreExec: []fism.Step{enabledStep(store, notifierPluginID, "notifier plugin store is nil")},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, step *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "notifier plugin store is nil"}
			}
			if step == nil || step.Registry() == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "notifier plugin registry is nil"}
			}
			if err := store.ReconcileRouterNotifiers(ctx, step.Registry(), event); err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
