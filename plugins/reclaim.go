package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const reclaimPluginID = "reclaim"

// ReclaimStore applies the legacy stale-task reclaim policy at a clock tick.
type ReclaimStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterReclaim(context.Context, router.Event) (int, error)
}

// ReclaimPlugin reclaims eligible legacy claims after a presence snapshot.
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
		Exec: []router.Step{func(ctx context.Context, event router.Event, _ *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "reclaim plugin store is nil"}
			}
			if _, err := store.ReconcileRouterReclaim(ctx, event); err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
