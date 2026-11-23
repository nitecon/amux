package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const nativeDeliveryPluginID = "native-delivery"

type NativeDeliveryStore interface {
	ReconcileNativeDelivery(context.Context, *router.Registry, router.Event) (bool, error)
}

// NativeDeliveryPlugin routes durable notifications into an already-bound
// native conversation. The router delivery itself supplies retry/backoff.
func NativeDeliveryPlugin(store NativeDeliveryStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{ID: nativeDeliveryPluginID, Subscriptions: []router.Subscription{{Event: NotifyEventID, MinVersion: 1, MaxVersion: 1}}},
		Exec: []router.Step{func(ctx context.Context, event router.Event, step *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "native delivery store is nil"}
			}
			if step == nil || step.Registry() == nil {
				return router.Outcome{Kind: router.Fail, Error: "native delivery registry is nil"}
			}
			pending, err := store.ReconcileNativeDelivery(ctx, step.Registry(), event)
			if err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			if pending {
				return router.Outcome{Kind: router.Fail, Error: "native notification is waiting for its receiver or adoption"}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
