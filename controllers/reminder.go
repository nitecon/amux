package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
)

const reminderPluginID = "reminder"

// ReminderStore provides settings and transactional reminder reconciliation.
type ReminderStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterReminders(context.Context, *fism.Registry, fism.Event) error
}

// ReminderController handles periodic reminders on clock ticks and restart
// continuity on client heartbeats after presence has accepted the report.
func ReminderController(store ReminderStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: reminderPluginID,
			Subscriptions: []fism.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1, DependsOn: []string{presencePluginID},
			}, {
				Event: ClientHeartbeatID, MinVersion: 1, MaxVersion: 1, DependsOn: []string{presencePluginID},
			}},
		},
		PreExec: []fism.Step{func(ctx context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
			if event.ID == ClientHeartbeatID {
				return fism.Outcome{Kind: fism.Continue}
			}
			return enabledStep(store, reminderPluginID, "reminder plugin store is nil")(ctx, event, nil)
		}},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, step *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "reminder plugin store is nil"}
			}
			if step == nil || step.Registry() == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "reminder plugin registry is nil"}
			}
			if err := store.ReconcileRouterReminders(ctx, step.Registry(), event); err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
