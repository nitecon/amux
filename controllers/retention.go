package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
	"github.com/rs/zerolog/log"
)

const (
	RetentionStartupID = "retention.startup"
	retentionPluginID  = "retention"
)

// RetentionStartupDefinition declares the local startup trigger for retention.
func RetentionStartupDefinition() fism.EventDefinition {
	return fism.EventDefinition{ID: RetentionStartupID, Version: 1, Validate: fism.Object}
}

// RetentionStore reconciles retained messages and router history for an event.
type RetentionStore interface {
	ReconcileRouterRetention(ctx context.Context, event fism.Event, retentionDays, botRetentionDays uint64) (bool, int64, error)
}

// RetentionController performs retention reconciliation on startup and clock ticks.
func RetentionController(store RetentionStore, retentionDays, botRetentionDays uint64) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: retentionPluginID,
			Subscriptions: []fism.Subscription{{
				Event: RetentionStartupID, MinVersion: 1, MaxVersion: 1,
			}, {
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
			}},
		},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "retention plugin store is nil"}
			}
			ran, deleted, err := store.ReconcileRouterRetention(ctx, event, retentionDays, botRetentionDays)
			if err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			if ran {
				log.Info().Ctx(ctx).Int64("count", deleted).Msg("Purged old messages")
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
