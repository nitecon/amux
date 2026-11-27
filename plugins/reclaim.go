package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const reclaimPluginID = "reclaim"

// ReclaimStore reconciles activity-driven recovery at a clock tick.
type ReclaimStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterProgress(context.Context, *router.Registry, router.Event) error
}

// ReclaimPlugin stalls, pings and requeues quiet work after a presence snapshot.
func ReclaimPlugin(store ReclaimStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: reclaimPluginID,
			Subscriptions: []router.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
				DependsOn: []string{"presence"},
			}},
		},
		PreExec: []router.Step{enabledStep(store, reclaimPluginID, "reclaim plugin store is nil")},
		Exec: []router.Step{func(ctx context.Context, event router.Event, step *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "reclaim plugin store is nil"}
			}
			var registry *router.Registry
			if step != nil {
				registry = step.Registry()
			}
			if err := store.ReconcileRouterProgress(ctx, registry, event); err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
