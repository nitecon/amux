package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const reminderPluginID = "reminder"

// ReminderStore provides settings and transactional reminder reconciliation.
type ReminderStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterReminders(context.Context, *router.Registry, router.Event) error
}

// ReminderPlugin handles periodic reminders on clock ticks and restart
// continuity on client heartbeats after presence has accepted the report.
func ReminderPlugin(store ReminderStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: reminderPluginID,
			Subscriptions: []router.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1, DependsOn: []string{presencePluginID},
			}, {
				Event: ClientHeartbeatID, MinVersion: 1, MaxVersion: 1, DependsOn: []string{presencePluginID},
			}},
		},
		PreExec: []router.Step{func(ctx context.Context, event router.Event, _ *router.StepContext) router.Outcome {
			if event.ID == ClientHeartbeatID {
				return router.Outcome{Kind: router.Continue}
			}
			return enabledStep(store, reminderPluginID, "reminder plugin store is nil")(ctx, event, nil)
		}},
		Exec: []router.Step{func(ctx context.Context, event router.Event, step *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "reminder plugin store is nil"}
			}
			if step == nil || step.Registry() == nil {
				return router.Outcome{Kind: router.Fail, Error: "reminder plugin registry is nil"}
			}
			if err := store.ReconcileRouterReminders(ctx, step.Registry(), event); err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
