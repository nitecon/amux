package controllers

import (
	"context"
	"errors"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

const (
	presencePluginID   = "presence"
	presenceSourceName = "presence"
)

// PresenceStore owns the observed presence snapshot.
type PresenceStore interface {
	ReconcileRouterPresence(context.Context, *fism.Registry, fism.Event) error
}

// PresenceController reconciles presence when a clock event reaches the router.
func PresenceController(store PresenceStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: presencePluginID,
			Subscriptions: []fism.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: ClientHeartbeatID, MinVersion: 1, MaxVersion: 1,
			}, {
				Event: ClientActivityID, MinVersion: 1, MaxVersion: 1,
			}, {
				Event: LegacyActivityID, MinVersion: 1, MaxVersion: 1,
			}},
		},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, step *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "presence plugin store is nil"}
			}
			if step == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "presence plugin step context is nil"}
			}
			if err := store.ReconcileRouterPresence(ctx, step.Registry(), event); err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}

type PresenceSource interface {
	DrainRouterPresenceOutbox(context.Context, *fism.Registry, int) (int, error)
}

// RunPresenceSource reconciles the durable presence outbox at startup and on
// presence or clock notifications. It owns its event subscription and retry timer.
func RunPresenceSource(ctx context.Context, store PresenceSource, registry *fism.Registry, bus *eventbus.Bus) error {
	if ctx == nil || store == nil || registry == nil || bus == nil {
		return errors.New("presence source requires context, store, registry, and event bus")
	}
	return runReconciliationSource(ctx, registry, bus, presenceSourceName, store.DrainRouterPresenceOutbox, eventbus.PresenceChanged)
}
