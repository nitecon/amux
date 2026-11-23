package plugins

import (
	"context"
	"errors"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
)

const (
	presencePluginID   = "presence"
	presenceSourceName = "presence"
)

// PresenceStore owns presence reconciliation and its durable router outbox.
type PresenceStore interface {
	ReconcileRouterPresence(context.Context, *router.Registry, router.Event) error
	DrainRouterPresenceOutbox(context.Context, *router.Registry, int) (int, error)
}

// PresencePlugin reconciles presence when a clock event reaches the router.
func PresencePlugin(store PresenceStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: presencePluginID,
			Subscriptions: []router.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
			}, {
				Event: ClientHeartbeatID, MinVersion: 1, MaxVersion: 1,
			}, {
				Event: ClientActivityID, MinVersion: 1, MaxVersion: 1,
			}, {
				Event: LegacyActivityID, MinVersion: 1, MaxVersion: 1,
			}},
		},
		Exec: []router.Step{func(ctx context.Context, event router.Event, step *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "presence plugin store is nil"}
			}
			if step == nil {
				return router.Outcome{Kind: router.Fail, Error: "presence plugin step context is nil"}
			}
			if err := store.ReconcileRouterPresence(ctx, step.Registry(), event); err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}

// RunPresenceSource reconciles the durable presence outbox at startup and on
// presence or clock notifications. It owns its event subscription and retry timer.
func RunPresenceSource(ctx context.Context, store PresenceStore, registry *router.Registry, bus *eventbus.Bus) error {
	if ctx == nil || store == nil || registry == nil || bus == nil {
		return errors.New("presence source requires context, store, registry, and event bus")
	}
	return runReconciliationSource(ctx, registry, bus, presenceSourceName, eventbus.PresenceChanged, store.DrainRouterPresenceOutbox)
}
