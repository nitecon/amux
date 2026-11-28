package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
)

const reclaimPluginID = "reclaim"

// ReclaimStore reconciles activity-driven recovery at a clock tick.
type ReclaimStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterProgress(context.Context, *fism.Registry, fism.Event) error
}

// ReclaimController stalls, pings and requeues quiet work after a presence snapshot.
func ReclaimController(store ReclaimStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: reclaimPluginID,
			Subscriptions: []fism.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
				DependsOn: []string{"presence"},
			}},
		},
		PreExec: []fism.Step{enabledStep(store, reclaimPluginID, "reclaim plugin store is nil")},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, step *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "reclaim plugin store is nil"}
			}
			var registry *fism.Registry
			if step != nil {
				registry = step.Registry()
			}
			if err := store.ReconcileRouterProgress(ctx, registry, event); err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
