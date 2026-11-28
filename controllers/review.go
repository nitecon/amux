package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
)

const reviewPluginID = "review"

// ReviewStore provides the settings check and review reconciliation operation
// consumed by the review plugin.
type ReviewStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterReviews(context.Context, fism.Event) (bool, error)
}

// ReviewController reconciles router reviews for completed tasks.
func ReviewController(store ReviewStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: reviewPluginID,
			Subscriptions: []fism.Subscription{{
				Event: "task.completed", MinVersion: 1, MaxVersion: 1,
			}},
		},
		PreExec: []fism.Step{enabledStep(store, reviewPluginID, "review plugin store is nil")},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "review plugin store is nil"}
			}
			reconciled, err := store.ReconcileRouterReviews(ctx, event)
			if err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			if !reconciled {
				return fism.Outcome{Kind: fism.Stop}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
