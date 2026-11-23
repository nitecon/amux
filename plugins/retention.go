package plugins

import (
	"context"
	"log/slog"

	"github.com/nitecon/amux/router"
)

const (
	RetentionStartupID = "retention.startup"
	retentionPluginID  = "retention"
)

// RetentionStartupDefinition declares the local startup trigger for retention.
func RetentionStartupDefinition() router.EventDefinition {
	return router.EventDefinition{ID: RetentionStartupID, Version: 1, Validate: router.Object}
}

// RetentionStore reconciles retained messages and router history for an event.
type RetentionStore interface {
	ReconcileRouterRetention(ctx context.Context, event router.Event, retentionDays, botRetentionDays uint64) (bool, int64, error)
}

// RetentionPlugin performs retention reconciliation on startup and clock ticks.
func RetentionPlugin(store RetentionStore, retentionDays, botRetentionDays uint64) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: retentionPluginID,
			Subscriptions: []router.Subscription{{
				Event: RetentionStartupID, MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
			}},
		},
		Exec: []router.Step{func(ctx context.Context, event router.Event, _ *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "retention plugin store is nil"}
			}
			ran, deleted, err := store.ReconcileRouterRetention(ctx, event, retentionDays, botRetentionDays)
			if err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			if ran {
				slog.InfoContext(ctx, "Purged old messages", "count", deleted)
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
