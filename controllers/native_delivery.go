package controllers

import (
	"context"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

const nativeDeliveryPluginID = "native-delivery"

type NativeDeliveryStore interface {
	ReconcileNativeDelivery(context.Context, *fism.Registry, fism.Event) (bool, error)
}

// NativeDeliveryController routes durable notifications into an already-bound
// native conversation. The notification source revisits pending delivery.
func NativeDeliveryController(store NativeDeliveryStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{ID: nativeDeliveryPluginID, Subscriptions: []fism.Subscription{{Event: NotifyEventID, MinVersion: 1, MaxVersion: 1}}},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, step *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "native delivery store is nil"}
			}
			if step == nil || step.Registry() == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "native delivery registry is nil"}
			}
			pending, err := store.ReconcileNativeDelivery(ctx, step.Registry(), event)
			if err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			if pending {
				return fism.Outcome{Kind: fism.Fail, Error: "native notification is waiting for its receiver or adoption"}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}

// RunNotificationSource observes accepted notifications at startup and when
// recipient presence, journal publications or the clock change.
func RunNotificationSource(ctx context.Context, registry *fism.Registry, bus *eventbus.Bus, drain func(context.Context, *fism.Registry, int) (int, error)) error {
	return runReconciliationSource(ctx, registry, bus, "notifications", drain, eventbus.RouterChanged, eventbus.PresenceChanged)
}
