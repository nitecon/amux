package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const reviewPluginID = "review"

// ReviewStore provides the settings check and review reconciliation operation
// consumed by the review plugin.
type ReviewStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterReviews(context.Context, router.Event) (bool, error)
}

// ReviewPlugin reconciles router reviews for completed tasks.
func ReviewPlugin(store ReviewStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: reviewPluginID,
			Subscriptions: []router.Subscription{{
				Event: "task.completed", MinVersion: 1, MaxVersion: 1,
			}},
		},
		PreExec: []router.Step{enabledStep(store, reviewPluginID, "review plugin store is nil")},
		Exec: []router.Step{func(ctx context.Context, event router.Event, _ *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "review plugin store is nil"}
			}
			reconciled, err := store.ReconcileRouterReviews(ctx, event)
			if err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			if !reconciled {
				return router.Outcome{Kind: router.Stop}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
